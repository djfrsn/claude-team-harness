package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBuildHandoffSeedsThreadFromRootExchange(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	room := RoomScope("room-1")
	thread := ThreadScope("room-1", "root")
	now := time.Now().UTC()
	for _, message := range []Message{
		{ID: "root", Scope: room, Role: "user", Text: "Root question", CreatedAt: now},
		{ID: "agent:root", Scope: room, Role: "assistant", Text: "Root answer", InReplyTo: "root", CreatedAt: now.Add(time.Second)},
		{ID: "other", Scope: room, Role: "user", Text: "Unrelated room message", CreatedAt: now.Add(2 * time.Second)},
		{ID: "thread-1", Scope: thread, Role: "user", Text: "Thread detail", CreatedAt: now.Add(3 * time.Second)},
	} {
		if _, err := store.AddMessage(ctx, message); err != nil {
			t.Fatalf("AddMessage(%s): %v", message.ID, err)
		}
	}
	handoff, err := store.BuildHandoff(ctx, thread, "", 1, true, 10, 1000)
	if err != nil {
		t.Fatalf("BuildHandoff: %v", err)
	}
	for _, wanted := range []string{"Root question", "Root answer", "Thread detail"} {
		if !strings.Contains(handoff, wanted) {
			t.Fatalf("handoff lacks %q:\n%s", wanted, handoff)
		}
	}
	if strings.Contains(handoff, "Unrelated room message") {
		t.Fatalf("thread handoff includes unrelated room context:\n%s", handoff)
	}
}

func TestWebexQueueDeduplicatesAndClaims(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	event := WebexEvent{MessageID: "message-1", RoomID: "room-1"}
	first, err := store.EnqueueWebex(ctx, event)
	if err != nil || !first {
		t.Fatalf("first enqueue = (%v, %v), want inserted", first, err)
	}
	second, err := store.EnqueueWebex(ctx, event)
	if err != nil || second {
		t.Fatalf("second enqueue = (%v, %v), want duplicate", second, err)
	}
	claimed, ok, err := store.ClaimWebex(ctx, time.Now().Add(time.Second))
	if err != nil || !ok || claimed.MessageID != event.MessageID {
		t.Fatalf("ClaimWebex = (%+v, %v, %v), want message-1", claimed, ok, err)
	}
	if err := store.CompleteWebex(ctx, claimed.MessageID); err != nil {
		t.Fatalf("CompleteWebex: %v", err)
	}
	if _, ok, err := store.ClaimWebex(ctx, time.Now().Add(time.Minute)); err != nil || ok {
		t.Fatalf("claim after complete = (%v, %v), want no event", ok, err)
	}
}

func TestParseStoredTimeRejectsInvalidValue(t *testing.T) {
	if _, err := parseStoredTime("not-a-time"); err == nil {
		t.Fatal("parseStoredTime accepted an invalid timestamp")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
