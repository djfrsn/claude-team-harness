package webex

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/state"
)

type fakeAPI struct {
	mu       sync.Mutex
	messages map[string]Message
	posts    []Message
}

func (f *fakeAPI) GetMessage(_ context.Context, messageID string) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.messages[messageID], nil
}

func (f *fakeAPI) PostMessage(
	_ context.Context, roomID, parentID, text string,
) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	message := Message{
		ID: "posted-" + text, RoomID: roomID, ParentID: parentID, Text: text,
	}
	f.posts = append(f.posts, message)
	return message, nil
}

type fakeManager struct {
	inputs []conversation.Input
	result conversation.Result
}

func (f *fakeManager) Handle(
	_ context.Context, input conversation.Input,
) (conversation.Result, error) {
	f.inputs = append(f.inputs, input)
	if f.result.Reply != "" || f.result.Steered {
		return f.result, nil
	}
	return conversation.Result{Reply: "answer", StopReason: acp.StopEndTurn}, nil
}

func TestWebhookVerifiesSignatureAndQueuesOnce(t *testing.T) {
	store := openWebexStore(t)
	wakes := 0
	handler := Handler{Secret: "secret", Store: store, Wake: func() { wakes++ }}
	body := `{"resource":"messages","event":"created","data":{"id":"m1","roomId":"r1"}}`

	request := httptest.NewRequest(http.MethodPost, "/v1/webex/events", strings.NewReader(body))
	request.Header.Set("X-Spark-Signature", sign("secret", body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("valid webhook status = %d, want %d; body = %s", response.Code, http.StatusAccepted, response.Body.String())
	}

	duplicate := httptest.NewRequest(http.MethodPost, "/v1/webex/events", strings.NewReader(body))
	duplicate.Header.Set("X-Spark-Signature", sign("secret", body))
	handler.ServeHTTP(httptest.NewRecorder(), duplicate)
	if wakes != 2 {
		t.Fatalf("wake count = %d, want two harmless worker wakes", wakes)
	}
	event, ok, err := store.ClaimWebex(context.Background(), time.Now().Add(time.Second))
	if err != nil || !ok || event.MessageID != "m1" {
		t.Fatalf("queued event = (%+v, %v, %v), want m1", event, ok, err)
	}
	if _, ok, err := store.ClaimWebex(context.Background(), time.Now().Add(time.Second)); err != nil || ok {
		t.Fatalf("duplicate created another event: ok=%v err=%v", ok, err)
	}
}

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	handler := Handler{Secret: "secret", Store: openWebexStore(t)}
	request := httptest.NewRequest(http.MethodPost, "/v1/webex/events", strings.NewReader(`{}`))
	request.Header.Set("X-Spark-Signature", "bad")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestWorkerRoutesRoomAndThreadIndependently(t *testing.T) {
	store := openWebexStore(t)
	now := time.Now().UTC()
	api := &fakeAPI{messages: map[string]Message{
		"root":   {ID: "root", RoomID: "room", PersonID: "human", Text: "Root", Created: now},
		"room-2": {ID: "room-2", RoomID: "room", PersonID: "human", Text: "Main", Created: now.Add(time.Second)},
		"reply":  {ID: "reply", RoomID: "room", ParentID: "root", PersonID: "human", Text: "Thread", Created: now.Add(2 * time.Second)},
	}}
	manager := &fakeManager{result: conversation.Result{
		Reply: "answer", Persona: "project-manager", StopReason: acp.StopEndTurn,
	}}
	worker, err := NewWorker(WorkerConfig{
		API: api, BotPersonID: "bot", Store: store, Manager: manager, TurnTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	for _, event := range []state.WebexEvent{
		{MessageID: "room-2", RoomID: "room"},
		{MessageID: "reply", RoomID: "room"},
	} {
		if _, err := store.EnqueueWebex(context.Background(), event); err != nil {
			t.Fatalf("EnqueueWebex: %v", err)
		}
		if !worker.processNext(context.Background()) {
			t.Fatal("worker found no queued event")
		}
	}
	if len(manager.inputs) != 2 {
		t.Fatalf("manager input count = %d, want two", len(manager.inputs))
	}
	if got := manager.inputs[0].Scope.Key; got != "room:room" {
		t.Fatalf("main message scope = %q, want room:room", got)
	}
	if got := manager.inputs[1].Scope.Key; got != "thread:room:root" {
		t.Fatalf("thread message scope = %q, want thread:room:root", got)
	}
	if len(api.posts) != 2 || api.posts[0].ParentID != "" || api.posts[1].ParentID != "root" {
		t.Fatalf("posted replies = %+v, want a room reply and a threaded reply", api.posts)
	}
	if api.posts[0].Text != "[project-manager] answer" {
		t.Fatalf("Webex reply = %q, want persona label", api.posts[0].Text)
	}
	if _, ok, err := store.Message(context.Background(), "root"); err != nil || !ok {
		t.Fatalf("thread root saved = %v, err = %v", ok, err)
	}
}

func TestWorkerDoesNotPostSeparateReplyForSteering(t *testing.T) {
	store := openWebexStore(t)
	api := &fakeAPI{messages: map[string]Message{
		"steer": {ID: "steer", RoomID: "room", PersonID: "human", Text: "Use Friday"},
	}}
	manager := &fakeManager{result: conversation.Result{Persona: "project-manager", Steered: true}}
	worker, err := NewWorker(WorkerConfig{
		API: api, BotPersonID: "bot", Store: store, Manager: manager, TurnTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if _, err := store.EnqueueWebex(
		context.Background(), state.WebexEvent{MessageID: "steer", RoomID: "room"},
	); err != nil {
		t.Fatalf("EnqueueWebex: %v", err)
	}
	worker.processNext(context.Background())
	if len(api.posts) != 0 {
		t.Fatalf("steering posted %d standalone replies, want none", len(api.posts))
	}
}

func TestWorkerIgnoresBotMessages(t *testing.T) {
	store := openWebexStore(t)
	api := &fakeAPI{messages: map[string]Message{
		"bot-message": {ID: "bot-message", RoomID: "room", PersonID: "bot", Text: "answer"},
	}}
	manager := &fakeManager{}
	worker, err := NewWorker(WorkerConfig{
		API: api, BotPersonID: "bot", Store: store, Manager: manager, TurnTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if _, err := store.EnqueueWebex(
		context.Background(), state.WebexEvent{MessageID: "bot-message", RoomID: "room"},
	); err != nil {
		t.Fatalf("EnqueueWebex: %v", err)
	}
	worker.processNext(context.Background())
	if len(manager.inputs) != 0 || len(api.posts) != 0 {
		t.Fatalf("bot event reached manager or posted a reply: inputs=%d posts=%d", len(manager.inputs), len(api.posts))
	}
}

func sign(secret, body string) string {
	digest := hmac.New(sha1.New, []byte(secret))
	_, _ = digest.Write([]byte(body))
	return hex.EncodeToString(digest.Sum(nil))
}

func openWebexStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
