package team

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/mcpprofile"
	"github.com/your-company/claude-team-harness/internal/persona"
	"github.com/your-company/claude-team-harness/internal/runqueue"
	"github.com/your-company/claude-team-harness/internal/state"
)

type fakeAgent struct {
	name string
	mu   sync.Mutex
	next int
}

type steeringAgent struct {
	started     chan struct{}
	release     chan struct{}
	steers      chan string
	sessions    chan string
	promptCalls atomic.Int32
}

type recoveryAgent struct {
	mu           sync.Mutex
	sessionID    string
	promptErrors []error
	promptCalls  int
	loads        []string
	newSessions  int
}

type trackedCloser struct {
	closed atomic.Int32
}

func (f *recoveryAgent) NewSession(context.Context, string, []acp.MCPServer) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newSessions++
	return f.sessionID, nil
}

func (f *recoveryAgent) LoadSession(
	_ context.Context, _, sessionID string, _ []acp.MCPServer,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, sessionID)
	return nil
}

func (f *recoveryAgent) Prompt(context.Context, string, string) (acp.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.promptCalls
	f.promptCalls++
	if call < len(f.promptErrors) && f.promptErrors[call] != nil {
		return acp.Turn{}, f.promptErrors[call]
	}
	return acp.Turn{Reply: "completed", StopReason: acp.StopEndTurn}, nil
}

func (f *recoveryAgent) SupportsSteering() bool { return false }

func (f *recoveryAgent) Steer(context.Context, string, string) (acp.SteerOutcome, error) {
	return acp.SteerPromptRequired, nil
}

func (c *trackedCloser) Close() error {
	c.closed.Add(1)
	return nil
}

func (f *steeringAgent) NewSession(context.Context, string, []acp.MCPServer) (string, error) {
	return "steering-session", nil
}
func (f *steeringAgent) LoadSession(context.Context, string, string, []acp.MCPServer) error {
	return nil
}
func (f *steeringAgent) Prompt(ctx context.Context, _ string, _ string) (acp.Turn, error) {
	f.promptCalls.Add(1)
	close(f.started)
	select {
	case <-f.release:
		return acp.Turn{Reply: "steered reply", StopReason: acp.StopEndTurn}, nil
	case <-ctx.Done():
		return acp.Turn{}, ctx.Err()
	}
}
func (f *steeringAgent) SupportsSteering() bool { return true }
func (f *steeringAgent) Steer(_ context.Context, session, text string) (acp.SteerOutcome, error) {
	f.sessions <- session
	f.steers <- text
	return acp.SteerInjected, nil
}

func (f *fakeAgent) NewSession(context.Context, string, []acp.MCPServer) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return fmt.Sprintf("%s-%d", f.name, f.next), nil
}
func (f *fakeAgent) LoadSession(context.Context, string, string, []acp.MCPServer) error { return nil }
func (f *fakeAgent) Prompt(_ context.Context, _ string, text string) (acp.Turn, error) {
	return acp.Turn{Reply: f.name + ":" + text, StopReason: acp.StopEndTurn}, nil
}
func (f *fakeAgent) SupportsSteering() bool { return false }
func (f *fakeAgent) Steer(context.Context, string, string) (acp.SteerOutcome, error) {
	return acp.SteerPromptRequired, nil
}

func TestOnePersonaUsesQualifiedRuntimePath(t *testing.T) {
	runtime := newTestRuntime(t, []persona.Persona{{
		Name: "planner", DisplayName: "Planner", Description: "plans", Prompt: "Plan.",
		Default: true, Slots: 1,
	}})
	result, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: state.RoomScope("room"), MessageID: "one", Text: "hello",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.Persona != "planner" || !strings.Contains(result.Reply, "<persona>") {
		t.Fatalf("single-persona result = %+v", result)
	}
}

func TestMentionRoutesIndependentPersona(t *testing.T) {
	runtime := newTestRuntime(t, []persona.Persona{
		{Name: "planner", DisplayName: "Planner", Description: "plans", Prompt: "Plan.", Default: true, Slots: 1},
		{Name: "engineer", DisplayName: "Engineer", Description: "builds", Prompt: "Build.", Slots: 1},
	})
	result, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: state.RoomScope("room"), MessageID: "one", Text: "@engineer check it",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.Persona != "engineer" || !strings.HasPrefix(result.Reply, "engineer:") {
		t.Fatalf("routed result = %+v", result)
	}
}

func TestExplicitPersonasKeepIndependentSessions(t *testing.T) {
	runtime := newTestRuntime(t, []persona.Persona{
		{Name: "planner", DisplayName: "Planner", Description: "plans", Prompt: "Plan.", Default: true, Slots: 1},
		{Name: "engineer", DisplayName: "Engineer", Description: "builds", Prompt: "Build.", Slots: 1},
	})
	scope := state.RoomScope("room")
	planner, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: scope, MessageID: "planner-1", Text: "Remember red", Persona: "planner",
	})
	if err != nil {
		t.Fatalf("planner Handle: %v", err)
	}
	engineer, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: scope, MessageID: "engineer-1", Text: "Remember blue", Persona: "engineer",
	})
	if err != nil {
		t.Fatalf("engineer Handle: %v", err)
	}
	if planner.SessionID == engineer.SessionID ||
		!strings.HasPrefix(planner.SessionID, "planner-") ||
		!strings.HasPrefix(engineer.SessionID, "engineer-") {
		t.Fatalf("persona sessions = %q and %q", planner.SessionID, engineer.SessionID)
	}
}

func TestConversationKeepsItsAssignedSlot(t *testing.T) {
	runtime := newTestRuntime(t, []persona.Persona{{
		Name: "planner", DisplayName: "Planner", Description: "plans", Prompt: "Plan.",
		Default: true, Slots: 2,
	}})
	pool := runtime.pools["planner"]
	first := pool.slotFor("planner|room:one")
	if again := pool.slotFor("planner|room:one"); again != first {
		t.Fatal("conversation moved to a different ACP slot")
	}
	if second := pool.slotFor("planner|room:two"); second == first {
		t.Fatal("round-robin assignment did not use the second ACP slot")
	}
}

func TestUnavailableSlotRecreatesClientAndLoadsSavedSession(t *testing.T) {
	first := &recoveryAgent{
		sessionID:    "saved-session",
		promptErrors: []error{nil, fmt.Errorf("%w: process exited", acp.ErrAdapterUnavailable)},
	}
	replacement := &recoveryAgent{sessionID: "unused-session"}
	firstCloser := &trackedCloser{}
	replacementCloser := &trackedCloser{}
	starts := 0
	runtime := newRuntimeWithStart(t, func(
		context.Context, persona.Persona, []acp.MCPServer, func(acp.Event),
	) (conversation.SessionClient, io.Closer, error) {
		starts++
		switch starts {
		case 1:
			return first, firstCloser, nil
		case 2:
			if closed := firstCloser.closed.Load(); closed != 1 {
				t.Fatalf("first adapter close calls = %d, want 1 before replacement", closed)
			}
			return replacement, replacementCloser, nil
		default:
			t.Fatalf("adapter starts = %d, want at most 2", starts)
			return nil, nil, errors.New("unexpected adapter start")
		}
	})
	scope := state.RoomScope("room")
	if _, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: scope, MessageID: "first", Text: "Remember the Friday release",
	}); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	assigned := runtime.pools["planner"].slotFor("planner|" + scope.Key)
	if _, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: scope, MessageID: "failed", Text: "This turn loses its adapter",
	}); !errors.Is(err, acp.ErrAdapterUnavailable) {
		t.Fatalf("failed Handle error = %v, want adapter unavailable", err)
	}
	recovered, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: scope, MessageID: "recovered", Text: "Continue the plan",
	})
	if err != nil {
		t.Fatalf("recovered Handle: %v", err)
	}
	if recovered.SessionID != "saved-session" || recovered.Generation != 1 ||
		recovered.Reply != "completed" {
		t.Fatalf("recovered result = %+v", recovered)
	}
	if starts != 2 || replacement.newSessions != 0 ||
		len(replacement.loads) != 1 || replacement.loads[0] != "saved-session" {
		t.Fatalf("starts = %d, replacement opens = %d, loads = %v",
			starts, replacement.newSessions, replacement.loads)
	}
	if again := runtime.pools["planner"].slotFor("planner|" + scope.Key); again != assigned {
		t.Fatal("recovered conversation moved to a different slot")
	}
}

func TestOrdinaryTurnErrorKeepsClient(t *testing.T) {
	agent := &recoveryAgent{
		sessionID: "session-1", promptErrors: []error{errors.New("model refused turn")},
	}
	starts := 0
	runtime := newRuntimeWithStart(t, func(
		context.Context, persona.Persona, []acp.MCPServer, func(acp.Event),
	) (conversation.SessionClient, io.Closer, error) {
		starts++
		return agent, &trackedCloser{}, nil
	})
	scope := state.RoomScope("room")
	if _, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: scope, MessageID: "failed", Text: "Try once",
	}); err == nil {
		t.Fatal("first Handle succeeded, want ordinary turn error")
	}
	if _, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: scope, MessageID: "second", Text: "Try again",
	}); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if starts != 1 {
		t.Fatalf("adapter starts = %d, want 1", starts)
	}
}

func TestQueuedMessageSteersActiveRun(t *testing.T) {
	agent := &steeringAgent{
		started: make(chan struct{}), release: make(chan struct{}), steers: make(chan string, 1),
		sessions: make(chan string, 1),
	}
	runtime := newRuntimeWithClient(t, agent)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	queue, err := runqueue.New(ctx, runqueue.Config{
		Store: runtime.cfg.Store, Handler: runtime, Workers: 2, TurnTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New queue: %v", err)
	}
	t.Cleanup(func() { cancel(); <-queue.Done() })
	first, created, err := queue.Submit(ctx, conversation.Input{
		Scope: state.RoomScope("room"), MessageID: "first", Text: "Draft the report",
	})
	if err != nil || !created {
		t.Fatalf("submit first run = (%+v, %v, %v)", first, created, err)
	}
	select {
	case <-agent.started:
	case <-ctx.Done():
		t.Fatal("first run did not reach the ACP prompt")
	}
	second, created, err := queue.Submit(ctx, conversation.Input{
		Scope: state.RoomScope("room"), MessageID: "second", Text: "Focus on Friday",
	})
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("submit steering run = (%+v, %v, %v)", second, created, err)
	}
	steered, found, err := queue.Wait(ctx, second.ID)
	if err != nil || !found || steered.Status != "completed" ||
		!steered.Steered || steered.ActiveRunID != first.ID {
		t.Fatalf("steer result = %+v", steered)
	}
	if text := <-agent.steers; text != "Focus on Friday" {
		t.Fatalf("steer text = %q", text)
	}
	if session := <-agent.sessions; session != "steering-session" {
		t.Fatalf("steer session = %q", session)
	}
	message, found, err := runtime.cfg.Store.Message(ctx, "second")
	if err != nil || !found || message.Text != "Focus on Friday" {
		t.Fatalf("stored steering message = (%+v, %v, %v)", message, found, err)
	}
	close(agent.release)
	completed, found, err := queue.Wait(ctx, first.ID)
	if err != nil || !found || completed.Status != "completed" || completed.Reply != "steered reply" {
		t.Fatalf("first result = %+v", completed)
	}
	if calls := agent.promptCalls.Load(); calls != 1 {
		t.Fatalf("ACP prompt calls = %d, want 1", calls)
	}
}

func newTestRuntime(t *testing.T, members []persona.Persona) *Runtime {
	t.Helper()
	roster, err := persona.NewSet(members)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	store, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	profiles, err := mcpprofile.Load("", nil)
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	runtime, err := New(context.Background(), Config{
		Roster: roster, Profiles: profiles, Store: store, Cwd: t.TempDir(), MaxTurns: 10,
		MaxAgents: 4, Start: func(
			_ context.Context, member persona.Persona, _ []acp.MCPServer, _ func(acp.Event),
		) (conversation.SessionClient, io.Closer, error) {
			return &fakeAgent{name: member.Name}, io.NopCloser(strings.NewReader("")), nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(); _ = store.Close() })
	return runtime
}

func newRuntimeWithClient(t *testing.T, client conversation.SessionClient) *Runtime {
	return newRuntimeWithStart(t, func(
		context.Context, persona.Persona, []acp.MCPServer, func(acp.Event),
	) (conversation.SessionClient, io.Closer, error) {
		return client, io.NopCloser(strings.NewReader("")), nil
	})
}

func newRuntimeWithStart(t *testing.T, start StartFunc) *Runtime {
	t.Helper()
	roster, err := persona.NewSet([]persona.Persona{{
		Name: "planner", DisplayName: "Planner", Description: "plans", Prompt: "Plan.",
		Default: true, Slots: 1,
	}})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	store, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	profiles, err := mcpprofile.Load("", nil)
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	runtime, err := New(context.Background(), Config{
		Roster: roster, Profiles: profiles, Store: store, Cwd: t.TempDir(),
		MaxTurns: 10, MaxAgents: 1, Start: start,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(); _ = store.Close() })
	return runtime
}
