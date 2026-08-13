package team

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/mcpprofile"
	"github.com/your-company/claude-team-harness/internal/persona"
	"github.com/your-company/claude-team-harness/internal/state"
)

type fakeAgent struct {
	name string
	mu   sync.Mutex
	next int
}

type steeringAgent struct {
	started chan struct{}
	release chan struct{}
	steers  chan string
}

func (f *steeringAgent) NewSession(context.Context, string, []acp.MCPServer) (string, error) {
	return "steering-session", nil
}
func (f *steeringAgent) LoadSession(context.Context, string, string, []acp.MCPServer) error {
	return nil
}
func (f *steeringAgent) Prompt(context.Context, string, string) (acp.Turn, error) {
	close(f.started)
	<-f.release
	return acp.Turn{Reply: "steered reply", StopReason: acp.StopEndTurn}, nil
}
func (f *steeringAgent) SupportsSteering() bool { return true }
func (f *steeringAgent) Steer(_ context.Context, _ string, text string) (acp.SteerOutcome, error) {
	f.steers <- text
	close(f.release)
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

func TestActiveConversationAcceptsSteering(t *testing.T) {
	agent := &steeringAgent{
		started: make(chan struct{}), release: make(chan struct{}), steers: make(chan string, 1),
	}
	runtime := newRuntimeWithClient(t, agent)
	firstResult := make(chan conversation.Result, 1)
	firstError := make(chan error, 1)
	go func() {
		result, err := runtime.Handle(context.Background(), conversation.Input{
			Scope: state.RoomScope("room"), MessageID: "first", Text: "Draft the report",
		})
		firstResult <- result
		firstError <- err
	}()
	<-agent.started
	steered, err := runtime.Handle(context.Background(), conversation.Input{
		Scope: state.RoomScope("room"), MessageID: "second", Text: "Focus on Friday",
	})
	if err != nil {
		t.Fatalf("steer Handle: %v", err)
	}
	if !steered.Steered || steered.RunID == "" {
		t.Fatalf("steer result = %+v", steered)
	}
	if text := <-agent.steers; text != "Focus on Friday" {
		t.Fatalf("steer text = %q", text)
	}
	if err := <-firstError; err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if first := <-firstResult; first.RunID != steered.RunID || first.Reply != "steered reply" {
		t.Fatalf("first result = %+v; steer = %+v", first, steered)
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
		MaxTurns: 10, MaxAgents: 1,
		Start: func(
			context.Context, persona.Persona, []acp.MCPServer, func(acp.Event),
		) (conversation.SessionClient, io.Closer, error) {
			return client, io.NopCloser(strings.NewReader("")), nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(); _ = store.Close() })
	return runtime
}
