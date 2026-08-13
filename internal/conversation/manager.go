// Package conversation routes durable room and thread conversations to ACP
// sessions.
package conversation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/state"
)

const (
	maxHandoffMessages = 24
	maxHandoffChars    = 16_000
)

type SessionClient interface {
	NewSession(context.Context, string, []acp.MCPServer) (string, error)
	LoadSession(context.Context, string, string, []acp.MCPServer) error
	Prompt(context.Context, string, string) (acp.Turn, error)
	SupportsSteering() bool
	Steer(context.Context, string, string) (acp.SteerOutcome, error)
}

type Mode string

const (
	ModeContinue Mode = "continue"
	ModeFresh    Mode = "fresh"
)

func ParseMode(value string) (Mode, error) {
	if value == "" {
		return ModeContinue, nil
	}
	mode := Mode(value)
	switch mode {
	case ModeContinue, ModeFresh:
		return mode, nil
	default:
		return "", fmt.Errorf("session mode must be continue or fresh, got %q", value)
	}
}

type Input struct {
	Scope     state.Scope
	MessageID string
	SenderID  string
	Text      string
	Mode      Mode
	CreatedAt time.Time
	Persona   string
}

type Result struct {
	Reply      string
	StopReason acp.StopReason
	SessionID  string
	Generation int
	Cached     bool
	Persona    string
	RunID      string
	Steered    bool
}

type Config struct {
	Client        SessionClient
	Store         *state.Store
	Cwd           string
	Servers       []acp.MCPServer
	MaxTurns      int
	Persona       string
	PersonaPrompt string
	OnActive      func(runID, sessionID string, active bool)
	Logf          func(string, ...any)
}

type Manager struct {
	cfg Config

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	loaded map[string]string
	active map[string]activeTurn
}

type activeTurn struct {
	RunID        string
	SessionID    string
	ContextEpoch int
}

func New(cfg Config) (*Manager, error) {
	if cfg.Client == nil || cfg.Store == nil || cfg.Cwd == "" {
		return nil, errors.New("conversation manager needs a client, store, and working directory")
	}
	if cfg.MaxTurns <= 0 {
		return nil, errors.New("conversation manager max turns must be positive")
	}
	if cfg.Persona == "" {
		cfg.Persona = "default"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Manager{
		cfg: cfg, locks: make(map[string]*sync.Mutex), loaded: make(map[string]string),
		active: make(map[string]activeTurn),
	}, nil
}

func (m *Manager) Handle(ctx context.Context, input Input) (Result, error) {
	input.Text = strings.TrimSpace(input.Text)
	if input.Scope.Key == "" || input.Scope.RoomID == "" || input.Text == "" {
		return Result{}, errors.New("message needs a scope and text")
	}
	mode, err := ParseMode(string(input.Mode))
	if err != nil {
		return Result{}, err
	}
	if input.MessageID == "" {
		input.MessageID, err = randomID("message")
		if err != nil {
			return Result{}, err
		}
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}

	lock := m.scopeLock(input.Scope.Key)
	lock.Lock()
	defer lock.Unlock()

	_, messageExists, err := m.cfg.Store.Message(ctx, input.MessageID)
	if err != nil {
		return Result{}, err
	}
	if messageExists {
		reply, ok, err := m.cfg.Store.ReplyTo(ctx, input.MessageID)
		if err != nil {
			return Result{}, err
		}
		if ok {
			conversation, _, err := m.cfg.Store.Conversation(ctx, input.Scope)
			return Result{
				Reply: reply, StopReason: acp.StopEndTurn,
				SessionID: conversation.ACPSessionID, Generation: conversation.Generation,
				Cached: true, Persona: m.cfg.Persona,
			}, err
		}
	}

	conversation, exists, err := m.cfg.Store.Conversation(ctx, input.Scope)
	if err != nil {
		return Result{}, err
	}

	prompt := input.Text
	sessionID := ""
	generation := 1
	contextEpoch := 1
	completedTurns := 0
	openReason := "new conversation"

	if exists {
		generation = conversation.Generation
		contextEpoch = conversation.ContextEpoch
		if contextEpoch <= 0 {
			contextEpoch = 1
		}
		completedTurns = conversation.CompletedTurns
		switch {
		case mode == ModeFresh:
			generation++
			contextEpoch++
			completedTurns = 0
			openReason = "fresh request"
		case conversation.CompletedTurns >= m.cfg.MaxTurns:
			generation++
			completedTurns = 0
			openReason = "turn limit"
		case conversation.ACPSessionID != "":
			sessionID = conversation.ACPSessionID
		}
	}
	includeThreadRoot := input.Scope.RootMessageID != "" && contextEpoch == 1 && mode != ModeFresh
	handoff, err := m.cfg.Store.BuildHandoff(
		ctx, input.Scope, input.MessageID, contextEpoch, includeThreadRoot,
		maxHandoffMessages, maxHandoffChars,
	)
	if err != nil {
		return Result{}, err
	}
	includeHandoff := handoff != "" && mode != ModeFresh

	if sessionID != "" && !m.isLoaded(input.Scope.Key, sessionID) {
		err := m.cfg.Client.LoadSession(ctx, m.cfg.Cwd, sessionID, m.cfg.Servers)
		if err == nil {
			m.markLoaded(input.Scope.Key, sessionID)
			m.cfg.Logf("conversation %s resumed ACP session %s", input.Scope.Key, sessionID)
		} else if acp.IsSessionMissing(err) {
			m.cfg.Logf("conversation %s ACP session is missing; opening a replacement", input.Scope.Key)
			sessionID = ""
			generation++
			completedTurns = 0
			openReason = "missing saved session"
		} else {
			return Result{}, fmt.Errorf("resume conversation %s: %w", input.Scope.Key, err)
		}
	}

	if sessionID == "" {
		sessionID, err = m.cfg.Client.NewSession(ctx, m.cfg.Cwd, m.cfg.Servers)
		if err != nil {
			return Result{}, fmt.Errorf("open conversation %s: %w", input.Scope.Key, err)
		}
		m.markLoaded(input.Scope.Key, sessionID)
		m.cfg.Logf("conversation %s opened ACP session %s (%s)", input.Scope.Key, sessionID, openReason)
		if includeHandoff {
			prompt = continuationPrompt(handoff, input.Text)
		}
		prompt = personaPrompt(m.cfg.PersonaPrompt, prompt)
		conversation = state.Conversation{
			Scope: input.Scope, ACPSessionID: sessionID, Generation: generation,
			ContextEpoch: contextEpoch, CompletedTurns: completedTurns, Handoff: handoff,
		}
		if err := m.cfg.Store.PutConversation(ctx, conversation); err != nil {
			return Result{}, err
		}
	}
	if !messageExists {
		if _, err := m.cfg.Store.AddMessage(ctx, state.Message{
			ID: input.MessageID, Scope: input.Scope, Role: "user",
			SenderID: input.SenderID, Text: input.Text, ContextEpoch: contextEpoch,
			CreatedAt: input.CreatedAt,
		}); err != nil {
			return Result{}, err
		}
	}

	turn, runID, err := m.runTurn(ctx, input, sessionID, prompt, contextEpoch)
	if err != nil {
		return Result{}, err
	}
	if _, err := m.cfg.Store.AddMessage(ctx, state.Message{
		ID: "agent:" + input.MessageID, Scope: input.Scope, Role: "assistant",
		Text: turn.Reply, InReplyTo: input.MessageID, ContextEpoch: contextEpoch,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return Result{}, err
	}
	completedTurns++
	if turn.StopReason == acp.StopMaxTokens || turn.StopReason == acp.StopMaxTurnRequests {
		completedTurns = m.cfg.MaxTurns
	}
	handoff, err = m.cfg.Store.BuildHandoff(
		ctx, input.Scope, "", contextEpoch, includeThreadRoot,
		maxHandoffMessages, maxHandoffChars,
	)
	if err != nil {
		return Result{}, err
	}
	if err := m.cfg.Store.PutConversation(ctx, state.Conversation{
		Scope: input.Scope, ACPSessionID: sessionID, Generation: generation,
		ContextEpoch: contextEpoch, CompletedTurns: completedTurns, Handoff: handoff,
	}); err != nil {
		return Result{}, err
	}
	if err := m.cfg.Store.FinishRun(ctx, runID, sessionID, turn.Reply, nil); err != nil {
		return Result{}, err
	}
	return Result{
		Reply: turn.Reply, StopReason: turn.StopReason,
		SessionID: sessionID, Generation: generation, Persona: m.cfg.Persona, RunID: runID,
	}, nil
}

func (m *Manager) runTurn(
	ctx context.Context, input Input, sessionID, prompt string, contextEpoch int,
) (acp.Turn, string, error) {
	runID, err := randomID("run")
	if err != nil {
		return acp.Turn{}, "", err
	}
	if err := m.cfg.Store.StartRun(ctx, state.Run{
		ID: runID, ScopeKey: input.Scope.Key, Persona: m.cfg.Persona,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		return acp.Turn{}, "", err
	}
	m.setActive(input.Scope.Key, activeTurn{
		RunID: runID, SessionID: sessionID, ContextEpoch: contextEpoch,
	})
	if m.cfg.OnActive != nil {
		m.cfg.OnActive(runID, sessionID, true)
	}
	turn, turnErr := m.cfg.Client.Prompt(ctx, sessionID, prompt)
	m.clearActive(input.Scope.Key)
	if m.cfg.OnActive != nil {
		m.cfg.OnActive(runID, sessionID, false)
	}
	if turnErr != nil {
		if err := m.cfg.Store.FinishRun(
			context.WithoutCancel(ctx), runID, sessionID, "", turnErr,
		); err != nil {
			m.cfg.Logf("finish failed run %s: %v", runID, err)
		}
		return acp.Turn{}, runID, turnErr
	}
	return turn, runID, nil
}

func (m *Manager) Steer(ctx context.Context, input Input) (Result, bool, error) {
	if !m.cfg.Client.SupportsSteering() {
		return Result{}, false, nil
	}
	if input.MessageID != "" {
		_, exists, err := m.cfg.Store.Message(ctx, input.MessageID)
		if err != nil || exists {
			return Result{}, false, err
		}
	}
	active, found := m.activeTurn(input.Scope.Key)
	if !found {
		return Result{}, false, nil
	}
	if input.MessageID == "" {
		var err error
		input.MessageID, err = randomID("message")
		if err != nil {
			return Result{}, false, err
		}
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	added, err := m.cfg.Store.AddMessage(ctx, state.Message{
		ID: input.MessageID, Scope: input.Scope, Role: "user", SenderID: input.SenderID,
		Text: strings.TrimSpace(input.Text), ContextEpoch: active.ContextEpoch,
		CreatedAt: input.CreatedAt,
	})
	if err != nil || !added {
		return Result{}, false, err
	}
	outcome, err := m.cfg.Client.Steer(ctx, active.SessionID, input.Text)
	if err != nil || !outcome.Delivered() {
		return Result{}, false, err
	}
	return Result{
		Persona: m.cfg.Persona, RunID: active.RunID, SessionID: active.SessionID,
		Steered: true,
	}, true, nil
}

func (m *Manager) scopeLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[key] == nil {
		m.locks[key] = &sync.Mutex{}
	}
	return m.locks[key]
}

func (m *Manager) isLoaded(scopeKey, sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loaded[scopeKey] == sessionID
}

func (m *Manager) markLoaded(scopeKey, sessionID string) {
	m.mu.Lock()
	m.loaded[scopeKey] = sessionID
	m.mu.Unlock()
}

func (m *Manager) setActive(key string, active activeTurn) {
	m.mu.Lock()
	m.active[key] = active
	m.mu.Unlock()
}

func (m *Manager) clearActive(key string) {
	m.mu.Lock()
	delete(m.active, key)
	m.mu.Unlock()
}

func (m *Manager) activeTurn(key string) (activeTurn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	active, found := m.active[key]
	return active, found
}

func personaPrompt(persona, prompt string) string {
	if strings.TrimSpace(persona) == "" {
		return prompt
	}
	return "<persona>\n" + persona + "\n</persona>\n\n" + prompt
}

func continuationPrompt(handoff, message string) string {
	return "This Claude session continues an existing conversation. " +
		"Use the prior conversation as context.\n\n<prior_conversation>\n" + handoff +
		"\n</prior_conversation>\n\n<current_message>\n" + message +
		"\n</current_message>"
}

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create message ID: %w", err)
	}
	return prefix + ":" + hex.EncodeToString(value[:]), nil
}
