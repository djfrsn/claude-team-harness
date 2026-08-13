// Package webex verifies message webhooks and moves messages between Webex and
// the conversation manager.
package webex

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // Webex defines X-Spark-Signature as an HMAC-SHA1 digest.
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/state"
)

const defaultBaseURL = "https://webexapis.com/v1"

type Message struct {
	ID       string    `json:"id"`
	RoomID   string    `json:"roomId"`
	ParentID string    `json:"parentId"`
	PersonID string    `json:"personId"`
	Text     string    `json:"text"`
	Created  time.Time `json:"created"`
}

type Person struct {
	ID string `json:"id"`
}

type API interface {
	GetMessage(context.Context, string) (Message, error)
	PostMessage(context.Context, string, string, string) (Message, error)
}

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewClient(token string, httpClient *http.Client) (*Client, error) {
	return NewClientAt(defaultBaseURL, token, httpClient)
}

func NewClientAt(baseURL, token string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Webex bot token is required")
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("invalid Webex base URL: %w", err)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), token: token, httpClient: httpClient,
	}, nil
}

func (c *Client) Me(ctx context.Context) (Person, error) {
	var person Person
	if err := c.doJSON(ctx, http.MethodGet, "/people/me", nil, &person); err != nil {
		return Person{}, err
	}
	if person.ID == "" {
		return Person{}, errors.New("Webex people/me returned an empty ID")
	}
	return person, nil
}

func (c *Client) GetMessage(ctx context.Context, messageID string) (Message, error) {
	var message Message
	path := "/messages/" + url.PathEscape(messageID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &message); err != nil {
		return Message{}, err
	}
	if message.ID == "" || message.RoomID == "" {
		return Message{}, errors.New("Webex returned a message without an ID or room ID")
	}
	return message, nil
}

func (c *Client) PostMessage(
	ctx context.Context, roomID, parentID, text string,
) (Message, error) {
	payload := map[string]string{"roomId": roomID, "text": text}
	if parentID != "" {
		payload["parentId"] = parentID
	}
	var message Message
	if err := c.doJSON(ctx, http.MethodPost, "/messages", payload, &message); err != nil {
		return Message{}, err
	}
	if message.ID == "" {
		return Message{}, errors.New("Webex created a message without an ID")
	}
	return message, nil
}

func (c *Client) doJSON(
	ctx context.Context, method, path string, input, output any,
) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Webex %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			return fmt.Errorf("read Webex %s %s error response: %w", method, path, err)
		}
		return fmt.Errorf("Webex %s %s returned %s: %s",
			method, path, response.Status, strings.TrimSpace(string(data)))
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode Webex %s %s: %w", method, path, err)
	}
	return nil
}

type Webhook struct {
	Resource string `json:"resource"`
	Event    string `json:"event"`
	Data     struct {
		ID     string `json:"id"`
		RoomID string `json:"roomId"`
	} `json:"data"`
}

func VerifySignature(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	digest := hmac.New(sha1.New, []byte(secret))
	_, _ = digest.Write(body)
	expected := digest.Sum(nil)
	return len(provided) == len(expected) && subtle.ConstantTimeCompare(provided, expected) == 1
}

type Handler struct {
	Secret string
	Store  *state.Store
	Wake   func()
}

func (h Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, 1<<20))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid webhook body"})
		return
	}
	if !VerifySignature(h.Secret, body, request.Header.Get("X-Spark-Signature")) {
		writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "invalid Webex signature"})
		return
	}
	var webhook Webhook
	if err := json.Unmarshal(body, &webhook); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid webhook JSON"})
		return
	}
	if webhook.Resource != "messages" || webhook.Event != "created" {
		writeJSON(response, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	if webhook.Data.ID == "" || webhook.Data.RoomID == "" {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "message webhook needs data.id and data.roomId"})
		return
	}
	_, err = h.Store.EnqueueWebex(request.Context(), state.WebexEvent{
		MessageID: webhook.Data.ID, RoomID: webhook.Data.RoomID,
	})
	if err != nil {
		writeJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not queue webhook"})
		return
	}
	if h.Wake != nil {
		h.Wake()
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "accepted"})
}

type MessageHandler interface {
	Handle(context.Context, conversation.Input) (conversation.Result, error)
}

type WorkerConfig struct {
	API         API
	BotPersonID string
	Store       *state.Store
	Manager     MessageHandler
	TurnTimeout time.Duration
	Concurrency int
	Logf        func(string, ...any)
}

type Worker struct {
	cfg  WorkerConfig
	wake chan struct{}
	done chan struct{}
}

func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.API == nil || cfg.Store == nil || cfg.Manager == nil || cfg.BotPersonID == "" {
		return nil, errors.New("Webex worker needs an API, bot person ID, store, and manager")
	}
	if cfg.TurnTimeout <= 0 {
		return nil, errors.New("Webex worker turn timeout must be positive")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	return &Worker{cfg: cfg, wake: make(chan struct{}, 1), done: make(chan struct{})}, nil
}

func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) Run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(w.cfg.Concurrency)
	for range w.cfg.Concurrency {
		go func() {
			defer workers.Done()
			w.runWorker(ctx)
		}()
	}
	w.Wake()
	workers.Wait()
	close(w.done)
}

func (w *Worker) runWorker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
		for w.processNext(ctx) {
		}
	}
}

func (w *Worker) Done() <-chan struct{} { return w.done }

func (w *Worker) processNext(ctx context.Context) bool {
	event, ok, err := w.cfg.Store.ClaimWebex(ctx, time.Now())
	if err != nil {
		w.cfg.Logf("claim Webex event: %v", err)
		return false
	}
	if !ok {
		return false
	}
	turnCtx, cancel := context.WithTimeout(ctx, w.cfg.TurnTimeout)
	err = w.process(turnCtx, event)
	cancel()
	if err == nil {
		if err := w.cfg.Store.CompleteWebex(ctx, event.MessageID); err != nil {
			w.cfg.Logf("complete Webex event %s: %v", event.MessageID, err)
		}
		return true
	}
	w.cfg.Logf("Webex event %s failed: %v", event.MessageID, err)
	if retryErr := w.cfg.Store.RetryWebex(ctx, event, err); retryErr != nil {
		w.cfg.Logf("record Webex retry %s: %v", event.MessageID, retryErr)
	}
	return true
}

func (w *Worker) process(ctx context.Context, event state.WebexEvent) error {
	message, err := w.cfg.API.GetMessage(ctx, event.MessageID)
	if err != nil {
		return err
	}
	if message.RoomID != event.RoomID {
		return fmt.Errorf("Webex message room does not match webhook room")
	}
	if message.PersonID == w.cfg.BotPersonID {
		return nil
	}
	scope := state.RoomScope(message.RoomID)
	if message.ParentID != "" {
		scope = state.ThreadScope(message.RoomID, message.ParentID)
		if err := w.saveThreadRoot(ctx, message); err != nil {
			return err
		}
	}
	result, err := w.cfg.Manager.Handle(ctx, conversation.Input{
		Scope: scope, MessageID: message.ID, SenderID: message.PersonID,
		Text: message.Text, Mode: conversation.ModeContinue, CreatedAt: message.Created,
	})
	if err != nil {
		return err
	}
	if result.Steered {
		return nil
	}
	reply := result.Reply
	if result.Persona != "" {
		reply = "[" + result.Persona + "] " + reply
	}
	_, err = w.cfg.API.PostMessage(ctx, message.RoomID, message.ParentID, reply)
	return err
}

func (w *Worker) saveThreadRoot(ctx context.Context, message Message) error {
	if _, ok, err := w.cfg.Store.Message(ctx, message.ParentID); err != nil || ok {
		return err
	}
	root, err := w.cfg.API.GetMessage(ctx, message.ParentID)
	if err != nil {
		return fmt.Errorf("fetch thread root: %w", err)
	}
	role := "user"
	if root.PersonID == w.cfg.BotPersonID {
		role = "assistant"
	}
	_, err = w.cfg.Store.AddMessage(ctx, state.Message{
		ID: root.ID, Scope: state.RoomScope(root.RoomID), Role: role,
		SenderID: root.PersonID, Text: root.Text, CreatedAt: root.Created,
	})
	return err
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		log.Printf("encode Webex response: %v", err)
	}
}
