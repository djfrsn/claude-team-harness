// Package acp implements the small part of the Agent Client Protocol that this
// service needs: initialize, session/new, session/prompt, updates, and tool
// permission replies.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	protocolVersion = 1
	maxFrame        = 4 << 20
	closeGrace      = 5 * time.Second
)

// ErrSessionMissing reports that an adapter cannot restore a saved session.
var ErrSessionMissing = errors.New("acp: saved session is missing")

type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopRefusal         StopReason = "refusal"
	StopCancelled       StopReason = "cancelled"
)

type SteerOutcome string

const (
	SteerInjected       SteerOutcome = "injected"
	SteerStartedNewTurn SteerOutcome = "startedNewTurn"
	SteerPromptRequired SteerOutcome = "promptRequired"
)

func (o SteerOutcome) Delivered() bool {
	return o == SteerInjected || o == SteerStartedNewTurn
}

type Event struct {
	SessionID  string
	Kind       string
	ToolCallID string
}

type PermissionPolicy string

const (
	PermissionDeny      PermissionPolicy = "deny"
	PermissionAllowOnce PermissionPolicy = "allow_once"
)

func ParsePermissionPolicy(value string) (PermissionPolicy, error) {
	policy := PermissionPolicy(value)
	switch policy {
	case PermissionDeny, PermissionAllowOnce:
		return policy, nil
	default:
		return "", fmt.Errorf("permission policy must be deny or allow_once, got %q", value)
	}
}

type MCPServer map[string]any

type Config struct {
	Command          string
	Dir              string
	Env              []string
	PermissionPolicy PermissionPolicy
	Stderr           io.Writer
	Logf             func(string, ...any)
	OnEvent          func(Event)
}

type Turn struct {
	Reply      string
	StopReason StopReason
}

type Client struct {
	policy PermissionPolicy
	logf   func(string, ...any)
	cmd    *exec.Cmd
	conn   *conn
	done   chan struct{}

	promptMu      sync.Mutex
	replyMu       sync.Mutex
	activeSession string
	reply         strings.Builder
	steering      bool
	onEvent       func(Event)
}

func Start(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Command == "" {
		return nil, errors.New("acp: adapter command is required")
	}
	if cfg.PermissionPolicy == "" {
		cfg.PermissionPolicy = PermissionDeny
	}
	if _, err := ParsePermissionPolicy(string(cfg.PermissionPolicy)); err != nil {
		return nil, err
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}

	cmd := exec.CommandContext(context.WithoutCancel(ctx), "sh", "-c", cfg.Command)
	cmd.Dir = cfg.Dir
	cmd.Env = append(cmd.Environ(), cfg.Env...)
	cmd.Env = append(cmd.Env, "PWD="+cfg.Dir)
	cmd.Stderr = cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: open adapter stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: open adapter stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start %q: %w", cfg.Command, err)
	}

	client := &Client{
		policy: cfg.PermissionPolicy, logf: cfg.Logf, cmd: cmd, done: make(chan struct{}),
		onEvent: cfg.OnEvent,
	}
	client.conn = newConn(stdout, stdin, client.serve)
	go func() {
		if err := cmd.Wait(); err != nil {
			cfg.Logf("ACP adapter exited: %v", err)
		}
		close(client.done)
	}()
	if err := client.initialize(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion int `json:"protocolVersion"`
		Meta            struct {
			Steering struct {
				Supported bool `json:"supported"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	err := c.callInto(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"clientInfo": map[string]string{
			"name": "claude-team-harness", "version": "0.1.0",
		},
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}, &result)
	if err != nil {
		return fmt.Errorf("acp: initialize: %w", err)
	}
	if result.ProtocolVersion != protocolVersion {
		return fmt.Errorf("acp: adapter protocol is %d; client protocol is %d", result.ProtocolVersion, protocolVersion)
	}
	c.steering = result.Meta.Steering.Supported
	return nil
}

func (c *Client) NewSession(ctx context.Context, cwd string, servers []MCPServer) (string, error) {
	if servers == nil {
		servers = []MCPServer{}
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.callInto(ctx, "session/new", map[string]any{
		"cwd": cwd, "mcpServers": servers,
	}, &result); err != nil {
		return "", fmt.Errorf("acp: session/new: %w", err)
	}
	if result.SessionID == "" {
		return "", errors.New("acp: session/new returned an empty session ID")
	}
	return result.SessionID, nil
}

// LoadSession restores an existing adapter session after this client process
// starts. The caller opens a replacement only when the adapter reports that the
// saved session is missing.
func (c *Client) LoadSession(
	ctx context.Context, cwd, sessionID string, servers []MCPServer,
) error {
	if servers == nil {
		servers = []MCPServer{}
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.callInto(ctx, "session/load", map[string]any{
		"cwd": cwd, "sessionId": sessionID, "mcpServers": servers,
	}, &result); err != nil {
		return fmt.Errorf("acp: session/load: %w", err)
	}
	if result.SessionID != "" && result.SessionID != sessionID {
		return fmt.Errorf("acp: session/load returned session %q, want %q", result.SessionID, sessionID)
	}
	return nil
}

func IsSessionMissing(err error) bool {
	var rpcErr *rpcError
	return errors.Is(err, ErrSessionMissing) || errors.As(err, &rpcErr) && rpcErr.Code == -32002
}

func (c *Client) Prompt(ctx context.Context, sessionID, text string) (Turn, error) {
	c.promptMu.Lock()
	defer c.promptMu.Unlock()

	c.replyMu.Lock()
	c.activeSession = sessionID
	c.reply.Reset()
	c.replyMu.Unlock()
	defer func() {
		c.replyMu.Lock()
		c.activeSession = ""
		c.replyMu.Unlock()
	}()

	response, err := c.conn.call("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		return Turn{}, fmt.Errorf("acp: session/prompt: %w", err)
	}

	var result callResult
	select {
	case <-ctx.Done():
		if err := c.conn.notify("session/cancel", map[string]string{"sessionId": sessionID}); err != nil {
			return Turn{}, fmt.Errorf("acp: cancel turn: %w", err)
		}
		result = <-response
	case result = <-response:
	}
	if result.err != nil {
		return Turn{}, fmt.Errorf("acp: session/prompt: %w", result.err)
	}
	var stopped struct {
		StopReason StopReason `json:"stopReason"`
	}
	if err := json.Unmarshal(result.payload, &stopped); err != nil {
		return Turn{}, fmt.Errorf("acp: decode prompt response: %w", err)
	}

	c.replyMu.Lock()
	reply := c.reply.String()
	c.replyMu.Unlock()
	return Turn{Reply: reply, StopReason: stopped.StopReason}, nil
}

func (c *Client) SupportsSteering() bool { return c.steering }

func (c *Client) Steer(ctx context.Context, sessionID, text string) (SteerOutcome, error) {
	if !c.steering {
		return "", errors.New("acp: adapter does not support steering")
	}
	var result struct {
		Outcome SteerOutcome `json:"outcome"`
	}
	err := c.callInto(ctx, "_session/steering", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
		"_meta": map[string]any{
			"steering": map[string]string{"idleBehavior": "promptRequired"},
		},
	}, &result)
	if err != nil {
		return "", fmt.Errorf("acp: steer session: %w", err)
	}
	switch result.Outcome {
	case SteerInjected, SteerStartedNewTurn, SteerPromptRequired:
		return result.Outcome, nil
	default:
		return "", fmt.Errorf("acp: unknown steering outcome %q", result.Outcome)
	}
}

func (c *Client) Close() error {
	if c.cmd.Process != nil {
		if err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL); err != nil &&
			!errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("acp: stop adapter: %w", err)
		}
	}
	select {
	case <-c.done:
		return nil
	case <-time.After(closeGrace):
		return fmt.Errorf("acp: adapter did not exit within %s", closeGrace)
	}
}

func (c *Client) callInto(ctx context.Context, method string, params, output any) error {
	response, err := c.conn.call(method, params)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-response:
		if result.err != nil {
			return result.err
		}
		return json.Unmarshal(result.payload, output)
	}
}

func (c *Client) serve(method string, params json.RawMessage, respond responder) {
	switch method {
	case "session/update":
		var update struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind       string `json:"sessionUpdate"`
				ToolCallID string `json:"toolCallId"`
				Content    struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(params, &update) != nil {
			return
		}
		if c.onEvent != nil {
			c.onEvent(Event{
				SessionID: update.SessionID, Kind: update.Update.Kind,
				ToolCallID: update.Update.ToolCallID,
			})
		}
		if update.Update.Kind != "agent_message_chunk" {
			return
		}
		c.replyMu.Lock()
		if update.SessionID == c.activeSession {
			c.reply.WriteString(update.Update.Content.Text)
		}
		c.replyMu.Unlock()
	case "session/request_permission":
		if respond != nil {
			respond(c.permission(params))
		}
	default:
		c.logf("acp: declined agent method %s", method)
		if respond != nil {
			respond(nil, &rpcError{Code: -32601, Message: "method not supported: " + method})
		}
	}
}

func (c *Client) permission(params json.RawMessage) (any, *rpcError) {
	var request struct {
		Options []struct {
			Kind     string `json:"kind"`
			OptionID string `json:"optionId"`
		} `json:"options"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid permission request"}
	}
	wanted := []string{"reject_once", "reject_always"}
	if c.policy == PermissionAllowOnce {
		wanted = []string{"allow_once"}
	}
	for _, kind := range wanted {
		for _, option := range request.Options {
			if option.Kind == kind {
				return map[string]any{"outcome": map[string]string{
					"outcome": "selected", "optionId": option.OptionID,
				}}, nil
			}
		}
	}
	return nil, &rpcError{Code: -32600, Message: "permission request has no option allowed by policy"}
}
