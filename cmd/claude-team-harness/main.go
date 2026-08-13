package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/mcpprofile"
	"github.com/your-company/claude-team-harness/internal/persona"
	"github.com/your-company/claude-team-harness/internal/state"
	"github.com/your-company/claude-team-harness/internal/team"
	"github.com/your-company/claude-team-harness/internal/webex"
)

const usage = `claude-team-harness runs a Claude Code team harness.

Usage:
  claude-team-harness prompt [flags] <message>
  claude-team-harness serve [flags]

Run "claude-team-harness <command> -help" for command flags.
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	switch args[0] {
	case "prompt":
		return runPrompt(ctx, args[1:], stdout)
	case "serve":
		return runServe(ctx, args[1:])
	case "help", "-help", "--help":
		_, err := io.WriteString(stdout, usage)
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type agentFlags struct {
	adapter          string
	cwd              string
	permissionPolicy string
	stateDB          string
	maxSessionTurns  int
	envFile          string
	personas         string
	mcpProfiles      string
	maxAgents        int
}

func addAgentFlags(flags *flag.FlagSet) *agentFlags {
	values := &agentFlags{}
	flags.StringVar(&values.adapter, "adapter", "claude-agent-acp", "ACP adapter command")
	flags.StringVar(&values.cwd, "cwd", "", "Claude session directory")
	flags.StringVar(&values.permissionPolicy, "permission-policy", "deny", "deny or allow_once")
	flags.StringVar(&values.stateDB, "state-db", ".claude-team-harness/state.db", "SQLite state database")
	flags.IntVar(&values.maxSessionTurns, "max-session-turns", 50, "turns before a context-preserving rotation")
	flags.StringVar(&values.envFile, "env-file", "", "environment file (default: <cwd>/.env)")
	flags.StringVar(&values.personas, "personas", "config/personas", "persona roster directory")
	flags.StringVar(&values.mcpProfiles, "mcp-profiles", "", "MCP profile JSON file")
	flags.IntVar(&values.maxAgents, "max-agents", 8, "maximum ACP adapter processes")
	return values
}

func runPrompt(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("prompt", flag.ContinueOnError)
	flags.SetOutput(stdout)
	values := addAgentFlags(flags)
	turnTimeout := flags.Duration("turn-timeout", 5*time.Minute, "maximum turn duration")
	conversationID := flags.String("conversation", "cli:default", "durable conversation key")
	sessionMode := flags.String("session-mode", "continue", "continue or fresh")
	personaName := flags.String("persona", "", "persona name (default: route by conversation)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("prompt needs a message")
	}
	runtime, err := openRuntime(ctx, *values)
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()

	turnCtx, cancel := context.WithTimeout(ctx, *turnTimeout)
	defer cancel()
	mode, err := conversation.ParseMode(*sessionMode)
	if err != nil {
		return err
	}
	turn, err := runtime.team.Handle(turnCtx, conversation.Input{
		Scope: state.Scope{Key: *conversationID, RoomID: "cli"},
		Text:  strings.Join(flags.Args(), " "), Mode: mode, Persona: *personaName,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, turn.Reply)
	return err
}

func runServe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	values := addAgentFlags(flags)
	listen := flags.String("listen", "127.0.0.1:8080", "HTTP listen address")
	turnTimeout := flags.Duration("turn-timeout", 5*time.Minute, "maximum turn duration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve does not accept positional arguments")
	}
	runtime, err := openRuntime(ctx, *values)
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()
	token := runtime.environment("CLAUDE_TEAM_HARNESS_API_TOKEN")
	if token == "" && !isLoopback(*listen) {
		return errors.New("CLAUDE_TEAM_HARNESS_API_TOKEN is required for a non-loopback listen address")
	}
	stopCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var webexHandler http.Handler
	var webexWorker *webex.Worker
	webexToken := runtime.environment("WEBEX_BOT_TOKEN")
	webexSecret := runtime.environment("WEBEX_WEBHOOK_SECRET")
	if (webexToken == "") != (webexSecret == "") {
		return errors.New("WEBEX_BOT_TOKEN and WEBEX_WEBHOOK_SECRET must be set together")
	}
	if webexToken != "" {
		baseURL := runtime.environment("WEBEX_API_BASE_URL")
		if baseURL == "" {
			baseURL = "https://webexapis.com/v1"
		}
		webexClient, err := webex.NewClientAt(baseURL, webexToken, nil)
		if err != nil {
			return err
		}
		person, err := webexClient.Me(ctx)
		if err != nil {
			return fmt.Errorf("identify Webex bot: %w", err)
		}
		worker, err := webex.NewWorker(webex.WorkerConfig{
			API: webexClient, BotPersonID: person.ID, Store: runtime.store,
			Manager: runtime.team, TurnTimeout: *turnTimeout, Logf: log.Printf,
		})
		if err != nil {
			return err
		}
		webexWorker = worker
		go worker.Run(stopCtx)
		webexHandler = webex.Handler{
			Secret: webexSecret, Store: runtime.store, Wake: worker.Wake,
		}
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           newHTTPHandler(token, *turnTimeout, runtime.team, webexHandler),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-stopCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(stopCtx), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown failed: %v", err)
		}
	}()
	log.Printf("Claude Team Harness listening on %s", *listen)
	serveErr := server.ListenAndServe()
	stop()
	if webexWorker != nil {
		select {
		case <-webexWorker.Done():
		case <-time.After(10 * time.Second):
			return errors.New("Webex worker did not stop within 10s")
		}
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", serveErr)
	}
	return nil
}

type runtime struct {
	team        *team.Runtime
	store       *state.Store
	environment func(string) string
}

func openRuntime(ctx context.Context, values agentFlags) (*runtime, error) {
	cwd := values.cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve session directory: %w", err)
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve session directory: %w", err)
	}
	environment, err := loadEnvironment(values.envFile, cwd)
	if err != nil {
		return nil, err
	}
	roster, err := persona.Load(values.personas)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(values.stateDB)
	if err != nil {
		return nil, err
	}
	profiles, err := mcpprofile.Load(values.mcpProfiles, environment)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	teamRuntime, err := team.New(ctx, team.Config{
		Roster: roster, Profiles: profiles, Store: store, Cwd: cwd,
		MaxTurns: values.maxSessionTurns, MaxAgents: values.maxAgents, Logf: log.Printf,
		Start: func(
			ctx context.Context, member persona.Persona, _ []acp.MCPServer, onEvent func(acp.Event),
		) (conversation.SessionClient, io.Closer, error) {
			command := member.Runtime
			if command == "" {
				command = values.adapter
			}
			policyName := member.PermissionPolicy
			if policyName == "" {
				policyName = values.permissionPolicy
			}
			policy, err := acp.ParsePermissionPolicy(policyName)
			if err != nil {
				return nil, nil, fmt.Errorf("persona %s: %w", member.Name, err)
			}
			adapterEnv := []string{}
			if token := environment("CLAUDE_OAUTH_TOKEN"); token != "" {
				adapterEnv = append(adapterEnv, "CLAUDE_OAUTH_TOKEN="+token)
			}
			client, err := acp.Start(ctx, acp.Config{
				Command: command, Dir: cwd, Env: adapterEnv, PermissionPolicy: policy,
				Stderr: os.Stderr, Logf: log.Printf, OnEvent: onEvent,
			})
			if err != nil {
				return nil, nil, err
			}
			return client, client, nil
		},
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &runtime{team: teamRuntime, store: store, environment: environment}, nil
}

func (r *runtime) Close() error {
	return errors.Join(r.team.Close(), r.store.Close())
}

func loadEnvironment(path, cwd string) (func(string) string, error) {
	if path == "" {
		path = filepath.Join(cwd, ".env")
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.Getenv, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read environment file: %w", err)
	}
	values, err := environmentValues(string(contents))
	if err != nil {
		return nil, err
	}
	return func(name string) string {
		if value := os.Getenv(name); value != "" {
			return value
		}
		return values[name]
	}, nil
}

func environmentValues(contents string) (map[string]string, error) {
	values := make(map[string]string)
	for lineNumber, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("environment file line %d needs NAME=VALUE", lineNumber+1)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
				value = value[1 : len(value)-1]
			}
		}
		if strings.HasPrefix(value, "\"") != strings.HasSuffix(value, "\"") ||
			strings.HasPrefix(value, "'") != strings.HasSuffix(value, "'") {
			return nil, fmt.Errorf("environment file line %d has an unmatched quote", lineNumber+1)
		}
		values[name] = value
	}
	return values, nil
}

type messageHandler interface {
	Handle(context.Context, conversation.Input) (conversation.Result, error)
}

type personaLister interface {
	Personas() []team.PersonaInfo
}

func newHTTPHandler(
	token string, turnTimeout time.Duration, manager messageHandler, webexHandler http.Handler,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/personas", func(response http.ResponseWriter, request *http.Request) {
		if token != "" && !authorized(request, token) {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		lister, ok := manager.(personaLister)
		if !ok {
			writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "persona listing unavailable"})
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"personas": lister.Personas()})
	})
	mux.HandleFunc("POST /v1/messages", func(response http.ResponseWriter, request *http.Request) {
		if token != "" && !authorized(request, token) {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		body := http.MaxBytesReader(response, request.Body, 1<<20)
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		var input struct {
			ConversationID string `json:"conversation_id"`
			RoomID         string `json:"room_id"`
			RootMessageID  string `json:"root_message_id"`
			MessageID      string `json:"message_id"`
			SenderID       string `json:"sender_id"`
			Text           string `json:"text"`
			SessionMode    string `json:"session_mode"`
			Persona        string `json:"persona"`
		}
		if err := decoder.Decode(&input); err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": "body must be a JSON object with text"})
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": "body must contain one JSON object"})
			return
		}
		input.Text = strings.TrimSpace(input.Text)
		if input.Text == "" {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": "text is required"})
			return
		}
		mode, err := conversation.ParseMode(input.SessionMode)
		if err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		scope := messageScope(input.ConversationID, input.RoomID, input.RootMessageID)
		turnCtx, cancel := context.WithTimeout(request.Context(), turnTimeout)
		defer cancel()
		turn, err := manager.Handle(turnCtx, conversation.Input{
			Scope: scope, MessageID: input.MessageID, SenderID: input.SenderID,
			Text: input.Text, Mode: mode, Persona: input.Persona,
		})
		if err != nil {
			var unknown persona.UnknownError
			if errors.As(err, &unknown) {
				writeJSON(response, http.StatusBadRequest, map[string]string{"error": unknown.Error()})
				return
			}
			log.Printf("agent turn failed: %v", err)
			writeJSON(response, http.StatusBadGateway, map[string]string{"error": "agent turn failed"})
			return
		}
		status := http.StatusOK
		if turn.Steered {
			status = http.StatusAccepted
		}
		writeJSON(response, status, map[string]any{
			"conversation_id": scope.Key, "persona": turn.Persona, "run_id": turn.RunID,
			"reply": turn.Reply, "stop_reason": string(turn.StopReason),
			"generation": turn.Generation, "cached": turn.Cached, "steered": turn.Steered,
		})
	})
	if webexHandler != nil {
		mux.Handle("POST /v1/webex/events", webexHandler)
	}
	return mux
}

func messageScope(conversationID, roomID, rootMessageID string) state.Scope {
	if roomID == "" {
		roomID = "api"
	}
	if conversationID != "" {
		return state.Scope{Key: conversationID, RoomID: roomID, RootMessageID: rootMessageID}
	}
	if rootMessageID != "" {
		return state.ThreadScope(roomID, rootMessageID)
	}
	return state.RoomScope(roomID)
}

func authorized(request *http.Request, token string) bool {
	const prefix = "Bearer "
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		log.Printf("encode HTTP response: %v", err)
	}
}

func isLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
