package conversation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/memory"
	"github.com/your-company/claude-team-harness/internal/state"
)

type fakeClient struct {
	nextSession int
	loadErr     error
	loads       []string
	prompts     []promptCall
}

type steerClient struct {
	fakeClient
	steers int
}

func (f *steerClient) SupportsSteering() bool { return true }

func (f *steerClient) Steer(context.Context, string, string) (acp.SteerOutcome, error) {
	f.steers++
	return acp.SteerInjected, nil
}

type promptCall struct {
	sessionID string
	text      string
}

func (f *fakeClient) NewSession(context.Context, string, []acp.MCPServer) (string, error) {
	f.nextSession++
	return fmt.Sprintf("session-%d", f.nextSession), nil
}

func (f *fakeClient) LoadSession(_ context.Context, _, sessionID string, _ []acp.MCPServer) error {
	f.loads = append(f.loads, sessionID)
	return f.loadErr
}

func (f *fakeClient) Prompt(_ context.Context, sessionID, text string) (acp.Turn, error) {
	f.prompts = append(f.prompts, promptCall{sessionID: sessionID, text: text})
	return acp.Turn{Reply: "reply to " + currentText(text), StopReason: acp.StopEndTurn}, nil
}

func (f *fakeClient) SupportsSteering() bool { return false }

func (f *fakeClient) Steer(context.Context, string, string) (acp.SteerOutcome, error) {
	return acp.SteerPromptRequired, nil
}

func currentText(prompt string) string {
	const start = "<current_message>\n"
	if at := strings.Index(prompt, start); at >= 0 {
		value := prompt[at+len(start):]
		return strings.TrimSuffix(value, "\n</current_message>")
	}
	return prompt
}

func TestRoomAndThreadKeepIndependentSessions(t *testing.T) {
	store := openStore(t)
	client := &fakeClient{}
	manager := newManager(t, store, client, 10)
	room := state.RoomScope("room-1")
	thread := state.ThreadScope("room-1", "root-1")

	roomFirst := handle(t, manager, Input{
		Scope: room, MessageID: "root-1", Text: "Release update?",
	})
	threadFirst := handle(t, manager, Input{
		Scope: thread, MessageID: "thread-1", Text: "What is blocked?",
	})
	roomSecond := handle(t, manager, Input{
		Scope: room, MessageID: "room-2", Text: "Main room follow-up",
	})

	if roomFirst.SessionID != roomSecond.SessionID {
		t.Fatalf("room sessions = %q then %q; the room must keep its session", roomFirst.SessionID, roomSecond.SessionID)
	}
	if threadFirst.SessionID == roomFirst.SessionID {
		t.Fatalf("thread session = room session %q; the thread needs independent context", threadFirst.SessionID)
	}
	threadPrompt := client.prompts[1].text
	if !strings.Contains(threadPrompt, "Release update?") || !strings.Contains(threadPrompt, "reply to Release update?") {
		t.Fatalf("first thread prompt lacks its root exchange:\n%s", threadPrompt)
	}
	if client.prompts[2].sessionID != roomFirst.SessionID {
		t.Fatalf("main room follow-up used %q, want %q", client.prompts[2].sessionID, roomFirst.SessionID)
	}
}

func TestTurnLimitRotatesWithPriorContext(t *testing.T) {
	store := openStore(t)
	client := &fakeClient{}
	manager := newManager(t, store, client, 2)
	scope := state.RoomScope("room-1")

	first := handle(t, manager, Input{Scope: scope, MessageID: "m1", Text: "First fact"})
	handle(t, manager, Input{Scope: scope, MessageID: "m2", Text: "Second fact"})
	third := handle(t, manager, Input{Scope: scope, MessageID: "m3", Text: "Continue"})

	if third.SessionID == first.SessionID || third.Generation != 2 {
		t.Fatalf("rotated turn = %+v, want a second-generation session", third)
	}
	prompt := client.prompts[2].text
	for _, text := range []string{"First fact", "reply to First fact", "Second fact", "Continue"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("rotation prompt lacks %q:\n%s", text, prompt)
		}
	}
}

func TestFreshStartsWithoutPriorContext(t *testing.T) {
	store := openStore(t)
	client := &fakeClient{}
	manager := newManager(t, store, client, 10)
	scope := state.RoomScope("room-1")

	first := handle(t, manager, Input{Scope: scope, MessageID: "m1", Text: "Old context"})
	fresh := handle(t, manager, Input{
		Scope: scope, MessageID: "m2", Text: "Clean start", Mode: ModeFresh,
	})
	if fresh.SessionID == first.SessionID || fresh.Generation != 2 {
		t.Fatalf("fresh turn = %+v, want a new second-generation session", fresh)
	}
	if prompt := client.prompts[1].text; prompt != "Clean start" {
		t.Fatalf("fresh prompt = %q, want only the current message", prompt)
	}
}

func TestMemoryLoadsOnlyWhenSessionOpens(t *testing.T) {
	store := openStore(t)
	memoryStore := openMemory(t)
	if _, err := memoryStore.Write(
		context.Background(), time.Now(), "planner", "The codeword is cedar-17.\n",
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	client := &fakeClient{}
	manager, err := New(Config{
		Client: client, Store: store, Memory: memoryStore, Cwd: t.TempDir(),
		MaxTurns: 10, Persona: "planner", PersonaPrompt: "Plan work.",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	scope := state.RoomScope("room-1")
	handle(t, manager, Input{Scope: scope, MessageID: "m1", Text: "First"})
	handle(t, manager, Input{Scope: scope, MessageID: "m2", Text: "Warm"})
	handle(t, manager, Input{
		Scope: scope, MessageID: "m3", Text: "Fresh", Mode: ModeFresh,
	})
	if first := client.prompts[0].text; !strings.Contains(first, "Plan work.") ||
		!strings.Contains(first, "[Your memory — 1 lines]") ||
		!strings.Contains(first, "cedar-17") {
		t.Fatalf("first prompt lacks persona memory:\n%s", first)
	}
	if warm := client.prompts[1].text; strings.Contains(warm, "cedar-17") {
		t.Fatalf("warm prompt repeated memory:\n%s", warm)
	}
	if fresh := client.prompts[2].text; !strings.Contains(fresh, "cedar-17") ||
		strings.Contains(fresh, "First") {
		t.Fatalf("fresh prompt = %q, want memory without prior conversation", fresh)
	}
}

func TestMemoryPruneInstructionReachesWarmTurns(t *testing.T) {
	store := openStore(t)
	memoryStore := openMemory(t)
	if _, err := memoryStore.Write(context.Background(), time.Now(), "planner",
		strings.Repeat("remembered line\n", memory.PruneLines+1)); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	client := &fakeClient{}
	manager, err := New(Config{
		Client: client, Store: store, Memory: memoryStore, Cwd: t.TempDir(),
		MaxTurns: 10, Persona: "planner",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	scope := state.RoomScope("room-1")
	handle(t, manager, Input{Scope: scope, MessageID: "m1", Text: "First"})
	handle(t, manager, Input{Scope: scope, MessageID: "m2", Text: "Warm"})
	for _, index := range []int{0, 1} {
		if !strings.Contains(client.prompts[index].text, "above the 450-line prune limit") {
			t.Fatalf("prompt %d lacks prune instruction", index)
		}
	}
}

func TestEmptyOrUnavailableMemoryDoesNotStopTurns(t *testing.T) {
	store := openStore(t)
	memoryStore := openMemory(t)
	client := &fakeClient{}
	logs := []string{}
	manager, err := New(Config{
		Client: client, Store: store, Memory: memoryStore, Cwd: t.TempDir(),
		MaxTurns: 10, Persona: "planner", Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handle(t, manager, Input{
		Scope: state.RoomScope("room-1"), MessageID: "m1", Text: "Empty memory",
	})
	if prompt := client.prompts[0].text; !strings.Contains(prompt, "It is empty") ||
		!strings.Contains(prompt, "memory write --file") {
		t.Fatalf("empty memory framing = %q", prompt)
	}
	if err := memoryStore.Close(); err != nil {
		t.Fatalf("close memory: %v", err)
	}
	handle(t, manager, Input{
		Scope: state.RoomScope("room-2"), MessageID: "m2", Text: "Unavailable memory",
	})
	if !slices.ContainsFunc(logs, func(line string) bool {
		return strings.Contains(line, "memory read planner")
	}) {
		t.Fatalf("memory failure logs = %v", logs)
	}
}

func TestRotationAfterFreshDoesNotRestoreOldContext(t *testing.T) {
	store := openStore(t)
	client := &fakeClient{}
	manager := newManager(t, store, client, 1)
	scope := state.RoomScope("room-1")

	handle(t, manager, Input{Scope: scope, MessageID: "m1", Text: "Old secret context"})
	handle(t, manager, Input{
		Scope: scope, MessageID: "m2", Text: "New baseline", Mode: ModeFresh,
	})
	handle(t, manager, Input{Scope: scope, MessageID: "m3", Text: "Rotate new baseline"})
	prompt := client.prompts[2].text
	if strings.Contains(prompt, "Old secret context") {
		t.Fatalf("post-fresh rotation restored old context:\n%s", prompt)
	}
	if !strings.Contains(prompt, "New baseline") {
		t.Fatalf("post-fresh rotation lacks the new baseline:\n%s", prompt)
	}
}

func TestNewManagerLoadsSavedSession(t *testing.T) {
	store := openStore(t)
	firstClient := &fakeClient{}
	firstManager := newManager(t, store, firstClient, 10)
	scope := state.RoomScope("room-1")
	first := handle(t, firstManager, Input{Scope: scope, MessageID: "m1", Text: "Before restart"})

	secondClient := &fakeClient{}
	secondManager := newManager(t, store, secondClient, 10)
	after := handle(t, secondManager, Input{Scope: scope, MessageID: "m2", Text: "After restart"})
	if len(secondClient.loads) != 1 || secondClient.loads[0] != first.SessionID {
		t.Fatalf("loaded sessions = %v, want [%s]", secondClient.loads, first.SessionID)
	}
	if after.SessionID != first.SessionID {
		t.Fatalf("post-restart session = %q, want %q", after.SessionID, first.SessionID)
	}
}

func TestMissingSavedSessionStartsReplacementWithHandoff(t *testing.T) {
	store := openStore(t)
	scope := state.RoomScope("room-1")
	firstManager := newManager(t, store, &fakeClient{}, 10)
	handle(t, firstManager, Input{Scope: scope, MessageID: "m1", Text: "The release is Friday"})
	first := handle(t, firstManager, Input{Scope: scope, MessageID: "m2", Text: "Review is pending"})
	client := &fakeClient{nextSession: 1, loadErr: acp.ErrSessionMissing}
	after := handle(t, newManager(t, store, client, 10), Input{Scope: scope, MessageID: "m3", Text: "What should we do next?"})
	saved, exists, err := store.Conversation(context.Background(), scope)
	if len(client.loads) != 1 || client.loads[0] != first.SessionID || client.nextSession != 2 ||
		after.SessionID == first.SessionID || after.Generation != first.Generation+1 || len(client.prompts) != 1 ||
		err != nil || !exists || saved.ACPSessionID != after.SessionID || saved.CompletedTurns != 1 || saved.ContextEpoch != 1 {
		t.Fatalf("replacement = %+v, persisted = %+v, loads %v, opens %d, prompts %d, exists %v, err %v", after, saved, client.loads, client.nextSession-1, len(client.prompts), exists, err)
	}
	prompt := client.prompts[0].text
	for _, text := range []string{
		"The release is Friday", "Review is pending", "What should we do next?",
	} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("replacement prompt lacks %q:\n%s", text, prompt)
		}
	}
}

func TestSavedSessionLoadFailureStopsTurn(t *testing.T) {
	store := openStore(t)
	scope := state.RoomScope("room-1")
	first := handle(t, newManager(t, store, &fakeClient{}, 10), Input{Scope: scope, MessageID: "m1", Text: "Keep this session"})
	loadErr := errors.New("adapter unavailable")
	client := &fakeClient{loadErr: loadErr}
	_, err := newManager(t, store, client, 10).Handle(context.Background(), Input{
		Scope: scope, MessageID: "m2", Text: "Do not run this turn",
	})
	if !errors.Is(err, loadErr) {
		t.Fatalf("Handle error = %v, want wrapped load error", err)
	}
	if len(client.loads) != 1 || client.loads[0] != first.SessionID ||
		client.nextSession != 0 || len(client.prompts) != 0 {
		t.Fatalf("load failure made calls: loads %v, opens %d, prompts %d", client.loads, client.nextSession, len(client.prompts))
	}
}

func TestDuplicateMessageReturnsStoredReply(t *testing.T) {
	store := openStore(t)
	client := &fakeClient{}
	manager := newManager(t, store, client, 10)
	input := Input{Scope: state.RoomScope("room-1"), MessageID: "same", Text: "Once"}
	first := handle(t, manager, input)
	second := handle(t, manager, input)
	if !second.Cached || second.Reply != first.Reply {
		t.Fatalf("duplicate result = %+v, want cached reply %q", second, first.Reply)
	}
	if len(client.prompts) != 1 {
		t.Fatalf("agent prompt count = %d, want one", len(client.prompts))
	}
}

func TestQueuedTurnKeepsProvidedRunID(t *testing.T) {
	store := openStore(t)
	manager := newManager(t, store, &fakeClient{}, 10)
	result := handle(t, manager, Input{
		Scope: state.RoomScope("room-1"), RunID: "run:queued",
		MessageID: "queued-message", Text: "Run later",
	})
	if result.RunID != "run:queued" {
		t.Fatalf("run ID = %q, want queued ID", result.RunID)
	}
}

func TestStoredMessageDoesNotSteerActiveTurn(t *testing.T) {
	store := openStore(t)
	client := &steerClient{}
	manager := newManager(t, store, client, 10)
	scope := state.RoomScope("room-1")
	if _, err := store.AddMessage(context.Background(), state.Message{
		ID: "same", Scope: scope, Role: "user", Text: "First delivery",
	}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	manager.setActive(scope.Key, activeTurn{
		RunID: "run-1", SessionID: "session-1", ContextEpoch: 1,
	})
	_, delivered, err := manager.Steer(context.Background(), Input{
		Scope: scope, MessageID: "same", Text: "Retry",
	})
	if err != nil || delivered || client.steers != 0 {
		t.Fatalf("stored steer = delivered %v, calls %d, err %v", delivered, client.steers, err)
	}
}

func openStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func openMemory(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memory.Open(context.Background(), t.TempDir()+"/memory.db")
	if err != nil {
		t.Fatalf("Open memory: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newManager(t *testing.T, store *state.Store, client SessionClient, maxTurns int) *Manager {
	t.Helper()
	manager, err := New(Config{
		Client: client, Store: store, Cwd: t.TempDir(), MaxTurns: maxTurns,
	})
	if err != nil {
		t.Fatalf("New manager: %v", err)
	}
	return manager
}

func handle(t *testing.T, manager *Manager, input Input) Result {
	t.Helper()
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	result, err := manager.Handle(context.Background(), input)
	if err != nil {
		t.Fatalf("Handle %q: %v", input.Text, err)
	}
	return result
}
