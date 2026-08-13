package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/persona"
	"github.com/your-company/claude-team-harness/internal/runqueue"
	"github.com/your-company/claude-team-harness/internal/state"
	"github.com/your-company/claude-team-harness/internal/team"
)

type personaLister interface {
	Personas() []team.PersonaInfo
}

type messageRequest struct {
	ConversationID string `json:"conversation_id"`
	RoomID         string `json:"room_id"`
	RootMessageID  string `json:"root_message_id"`
	MessageID      string `json:"message_id"`
	SenderID       string `json:"sender_id"`
	Text           string `json:"text"`
	SessionMode    string `json:"session_mode"`
	Persona        string `json:"persona"`
}

type runResponse struct {
	RunID          string `json:"run_id"`
	ActiveRunID    string `json:"active_run_id,omitempty"`
	Status         string `json:"status"`
	StatusURL      string `json:"status_url"`
	ConversationID string `json:"conversation_id"`
	Persona        string `json:"persona,omitempty"`
	Reply          string `json:"reply,omitempty"`
	StopReason     string `json:"stop_reason,omitempty"`
	Generation     int    `json:"generation,omitempty"`
	Cached         bool   `json:"cached,omitempty"`
	Steered        bool   `json:"steered,omitempty"`
	Error          string `json:"error,omitempty"`
}

func newHTTPHandler(
	token string, maxWait time.Duration, queue *runqueue.Service,
	personas personaLister, webexHandler http.Handler,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/personas", func(response http.ResponseWriter, request *http.Request) {
		if !authorize(response, request, token) {
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"personas": personas.Personas()})
	})
	mux.HandleFunc("POST /v1/messages", func(response http.ResponseWriter, request *http.Request) {
		if !authorize(response, request, token) {
			return
		}
		handleMessageSubmit(response, request, maxWait, queue)
	})
	mux.HandleFunc("GET /v1/runs/{runID}", func(response http.ResponseWriter, request *http.Request) {
		if !authorize(response, request, token) {
			return
		}
		run, found, err := queue.Run(request.Context(), request.PathValue("runID"))
		if err != nil {
			log.Printf("read message run: %v", err)
			writeJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not read run"})
			return
		}
		if !found {
			writeJSON(response, http.StatusNotFound, map[string]string{"error": "run not found"})
			return
		}
		writeJSON(response, http.StatusOK, responseForRun(run))
	})
	if webexHandler != nil {
		mux.Handle("POST /v1/webex/events", webexHandler)
	}
	return mux
}

func handleMessageSubmit(
	response http.ResponseWriter, request *http.Request,
	maxWait time.Duration, queue *runqueue.Service,
) {
	input, err := decodeMessageRequest(response, request)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	wait, err := preferredWait(request.Header.Get("Prefer"), maxWait)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	mode, err := conversation.ParseMode(input.SessionMode)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	scope := messageScope(input.ConversationID, input.RoomID, input.RootMessageID)
	run, created, err := queue.Submit(request.Context(), conversation.Input{
		Scope: scope, MessageID: input.MessageID, SenderID: input.SenderID,
		Text: input.Text, Mode: mode, Persona: input.Persona,
	})
	if err != nil {
		writeSubmitError(response, err)
		return
	}
	location := "/v1/runs/" + run.ID
	response.Header().Set("Location", location)
	if wait > 0 && !terminalRun(run.Status) {
		waitCtx, cancel := context.WithTimeout(request.Context(), wait)
		run, _, err = queue.Wait(waitCtx, run.ID)
		cancel()
		if err != nil {
			log.Printf("wait for message run: %v", err)
			writeJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not wait for run"})
			return
		}
	}
	status := http.StatusAccepted
	if terminalRun(run.Status) && (!created || wait > 0) {
		status = http.StatusOK
	} else {
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, responseForRun(run))
}

func decodeMessageRequest(response http.ResponseWriter, request *http.Request) (messageRequest, error) {
	body := http.MaxBytesReader(response, request.Body, 1<<20)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var input messageRequest
	if err := decoder.Decode(&input); err != nil {
		return messageRequest{}, errors.New("body must be a JSON object with text")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return messageRequest{}, errors.New("body must contain one JSON object")
	}
	input.Text = strings.TrimSpace(input.Text)
	if input.Text == "" {
		return messageRequest{}, errors.New("text is required")
	}
	return input, nil
}

func preferredWait(header string, maximum time.Duration) (time.Duration, error) {
	for _, directive := range strings.Split(header, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(directive), "=")
		if !found || strings.TrimSpace(name) != "wait" {
			continue
		}
		seconds, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || seconds < 0 {
			return 0, errors.New("prefer wait must be a non-negative whole number of seconds")
		}
		wait := time.Duration(seconds) * time.Second
		if wait > maximum {
			return 0, fmt.Errorf("prefer wait exceeds the %s maximum", maximum)
		}
		return wait, nil
	}
	return 0, nil
}

func writeSubmitError(response http.ResponseWriter, err error) {
	var unknown persona.UnknownError
	var conflict runqueue.ConflictError
	switch {
	case errors.As(err, &unknown):
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": unknown.Error()})
	case errors.As(err, &conflict):
		writeJSON(response, http.StatusConflict, map[string]string{"error": conflict.Error()})
	default:
		log.Printf("queue message failed: %v", err)
		writeJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not queue message"})
	}
}

func responseForRun(run state.MessageRun) runResponse {
	personaName := run.ResultPersona
	if personaName == "" {
		personaName = run.Persona
	}
	return runResponse{
		RunID: run.ID, ActiveRunID: run.ActiveRunID, Status: run.Status,
		StatusURL: "/v1/runs/" + run.ID, ConversationID: run.Scope.Key,
		Persona: personaName, Reply: run.Reply, StopReason: run.StopReason,
		Generation: run.Generation, Cached: run.Cached, Steered: run.Steered,
		Error: run.Error,
	}
}

func terminalRun(status string) bool { return status == "completed" || status == "failed" }

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

func authorize(response http.ResponseWriter, request *http.Request, token string) bool {
	if token == "" || authorized(request, token) {
		return true
	}
	writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	return false
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
