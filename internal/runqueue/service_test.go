package runqueue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/state"
)

type handlerFunc func(context.Context, conversation.Input) (conversation.Result, error)

func (f handlerFunc) Handle(
	ctx context.Context, input conversation.Input,
) (conversation.Result, error) {
	return f(ctx, input)
}

func TestServiceRecordsBoundedHandlerFailure(t *testing.T) {
	store := openStore(t)
	failure := errors.New("upstream failed: " + strings.Repeat("界", 1_000))
	service, cancel := startService(t, store, time.Second, handlerFunc(func(
		context.Context, conversation.Input,
	) (conversation.Result, error) {
		return conversation.Result{}, failure
	}))
	defer stopService(t, service, cancel)

	run := submitRun(t, service, "message-failure")
	failed := waitForRun(t, service, run.ID)
	if failed.Status != "failed" {
		t.Fatalf("run status = %q, want failed", failed.Status)
	}
	if !strings.HasPrefix(failed.Error, "upstream failed: ") {
		t.Fatalf("run error = %q, want useful failure prefix", failed.Error)
	}
	if len(failed.Error) > 2_000 || !utf8.ValidString(failed.Error) {
		t.Fatalf("run error has %d bytes and valid UTF-8 = %v, want at most 2000 valid bytes",
			len(failed.Error), utf8.ValidString(failed.Error))
	}
	if failed.CompletedAt.IsZero() {
		t.Fatal("failed run has no completion time")
	}
}

func TestServiceMarksTurnTimeoutFailed(t *testing.T) {
	store := openStore(t)
	started := make(chan struct{})
	service, cancel := startService(t, store, 20*time.Millisecond, handlerFunc(func(
		ctx context.Context, _ conversation.Input,
	) (conversation.Result, error) {
		close(started)
		<-ctx.Done()
		return conversation.Result{}, ctx.Err()
	}))
	defer stopService(t, service, cancel)

	run := submitRun(t, service, "message-timeout")
	awaitSignal(t, started, "handler start")
	failed := waitForRun(t, service, run.ID)
	if failed.Status != "failed" {
		t.Fatalf("run status = %q, want failed", failed.Status)
	}
	if failed.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("run error = %q, want %q", failed.Error, context.DeadlineExceeded)
	}
}

func TestServiceRequeuesActiveRunOnShutdown(t *testing.T) {
	store := openStore(t)
	started := make(chan struct{})
	service, cancel := startService(t, store, time.Second, handlerFunc(func(
		ctx context.Context, _ conversation.Input,
	) (conversation.Result, error) {
		close(started)
		<-ctx.Done()
		return conversation.Result{}, ctx.Err()
	}))

	run := submitRun(t, service, "message-shutdown")
	awaitSignal(t, started, "handler start")
	cancel()
	awaitSignal(t, service.Done(), "service shutdown")

	requeued, found, err := store.MessageRun(context.Background(), run.ID)
	if err != nil || !found {
		t.Fatalf("read requeued run = (%+v, %v, %v)", requeued, found, err)
	}
	if requeued.Status != "queued" || !requeued.StartedAt.IsZero() {
		t.Fatalf("requeued run = status %q, started %v; want queued with no start time",
			requeued.Status, requeued.StartedAt)
	}
}

func TestServiceCompletesPersistedRunningRunAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	firstStore, err := state.Open(path)
	if err != nil {
		t.Fatalf("open first state: %v", err)
	}
	t.Cleanup(func() { _ = firstStore.Close() })
	wanted := state.MessageRun{
		ID: "run-restart", MessageID: "message-restart", Scope: state.RoomScope("room-1"),
		Text: "status", Mode: string(conversation.ModeContinue),
	}
	if _, created, err := firstStore.QueueMessageRun(ctx, wanted); err != nil || !created {
		t.Fatalf("queue run = (%v, %v), want created", created, err)
	}
	claimed, found, err := firstStore.ClaimMessageRun(ctx)
	if err != nil || !found || claimed.ID != wanted.ID || claimed.Status != "running" {
		t.Fatalf("claim run = (%+v, %v, %v), want running %q", claimed, found, err, wanted.ID)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first state: %v", err)
	}

	restartedStore, err := state.Open(path)
	if err != nil {
		t.Fatalf("reopen state: %v", err)
	}
	t.Cleanup(func() { _ = restartedStore.Close() })
	recovered, found, err := restartedStore.MessageRun(ctx, wanted.ID)
	if err != nil || !found || recovered.Status != "queued" || !recovered.StartedAt.IsZero() {
		t.Fatalf("recovered run = (%+v, %v, %v), want queued with no start time",
			recovered, found, err)
	}

	received := make(chan conversation.Input, 1)
	service, cancel := startService(t, restartedStore, time.Second, handlerFunc(func(
		_ context.Context, input conversation.Input,
	) (conversation.Result, error) {
		received <- input
		return conversation.Result{RunID: input.RunID, Reply: "recovered"}, nil
	}))
	defer stopService(t, service, cancel)

	completed := waitForRun(t, service, wanted.ID)
	if completed.ID != wanted.ID || completed.Status != "completed" || completed.Reply != "recovered" {
		t.Fatalf("completed run = %+v, want same ID with recovered reply", completed)
	}
	select {
	case input := <-received:
		if input.RunID != wanted.ID {
			t.Fatalf("handler run ID = %q, want %q", input.RunID, wanted.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recovered handler input")
	}
}

func openStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func startService(
	t *testing.T, store *state.Store, timeout time.Duration, handler Handler,
) (*Service, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	service, err := New(ctx, Config{
		Store: store, Handler: handler, Workers: 1, TurnTimeout: timeout,
	})
	if err != nil {
		cancel()
		t.Fatalf("start service: %v", err)
	}
	return service, cancel
}

func stopService(t *testing.T, service *Service, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	awaitSignal(t, service.Done(), "service shutdown")
}

func submitRun(t *testing.T, service *Service, messageID string) state.MessageRun {
	t.Helper()
	run, created, err := service.Submit(context.Background(), conversation.Input{
		Scope: state.RoomScope("room-1"), MessageID: messageID, Text: "status",
	})
	if err != nil || !created {
		t.Fatalf("submit run = (%+v, %v, %v)", run, created, err)
	}
	return run
}

func waitForRun(t *testing.T, service *Service, runID string) state.MessageRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	run, found, err := service.Wait(ctx, runID)
	if err != nil || !found {
		t.Fatalf("wait for run = (%+v, %v, %v)", run, found, err)
	}
	if !terminal(run.Status) {
		t.Fatalf("run status = %q after wait, want terminal", run.Status)
	}
	return run
}

func awaitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
