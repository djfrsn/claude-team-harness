package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/persona"
	"github.com/your-company/claude-team-harness/internal/team"
)

type fakeMessageHandler func(context.Context, conversation.Input) (conversation.Result, error)

func (f fakeMessageHandler) Handle(
	ctx context.Context, input conversation.Input,
) (conversation.Result, error) {
	return f(ctx, input)
}

type fakeTeamHandler struct {
	handle   fakeMessageHandler
	personas []team.PersonaInfo
}

func (f fakeTeamHandler) Handle(ctx context.Context, input conversation.Input) (conversation.Result, error) {
	return f.handle(ctx, input)
}

func (f fakeTeamHandler) Personas() []team.PersonaInfo { return f.personas }

func TestMessageEndpointAuthenticatesAndReturnsTurn(t *testing.T) {
	handler := newHTTPHandler("test-token", time.Second,
		fakeMessageHandler(func(_ context.Context, input conversation.Input) (conversation.Result, error) {
			if input.Text != "daily report" {
				t.Fatalf("message text = %q, want daily report", input.Text)
			}
			if input.Scope.Key != "room:delivery" {
				t.Fatalf("conversation key = %q, want room:delivery", input.Scope.Key)
			}
			return conversation.Result{
				Reply: "all clear", StopReason: acp.StopEndTurn, Generation: 2,
				Persona: "project-manager", RunID: "run-1",
			}, nil
		}), nil)

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"text":"daily report"}`))
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorizedResponse.Code, http.StatusUnauthorized)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"room_id":"delivery","text":"daily report"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, `"reply":"all clear"`) ||
		!strings.Contains(body, `"persona":"project-manager"`) {
		t.Fatalf("response body = %s, want reply and stop reason", body)
	}
}

func TestMessageEndpointReturnsAcceptedForSteering(t *testing.T) {
	handler := newHTTPHandler("", time.Second,
		fakeMessageHandler(func(_ context.Context, input conversation.Input) (conversation.Result, error) {
			if input.Persona != "engineer" {
				t.Fatalf("persona = %q, want engineer", input.Persona)
			}
			return conversation.Result{
				Persona: "engineer", RunID: "run-active", Steered: true,
			}, nil
		}), nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"conversation_id":"release","persona":"engineer","text":"Use Friday"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"steered":true`) {
		t.Fatalf("steer response = %d %s", response.Code, response.Body.String())
	}
}

func TestPersonaEndpointListsEnabledPersonas(t *testing.T) {
	handler := newHTTPHandler("", time.Second, fakeTeamHandler{
		handle: func(context.Context, conversation.Input) (conversation.Result, error) {
			return conversation.Result{}, nil
		},
		personas: []team.PersonaInfo{
			{Name: "project-manager", DisplayName: "Project Manager", Default: true},
			{Name: "engineer", DisplayName: "Engineer"},
		},
	}, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/personas", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"engineer"`) {
		t.Fatalf("personas response = %d %s", response.Code, response.Body.String())
	}
}

func TestMessageEndpointRejectsUnknownPersona(t *testing.T) {
	handler := newHTTPHandler("", time.Second,
		fakeMessageHandler(func(context.Context, conversation.Input) (conversation.Result, error) {
			return conversation.Result{}, persona.UnknownError{Name: "writer"}
		}), nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"text":"hello","persona":"writer"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unknown persona") {
		t.Fatalf("unknown persona response = %d %s", response.Code, response.Body.String())
	}
}

func TestMessageEndpointRejectsUnknownInput(t *testing.T) {
	handler := newHTTPHandler("", time.Second,
		fakeMessageHandler(func(context.Context, conversation.Input) (conversation.Result, error) {
			t.Fatal("invalid input reached the agent")
			return conversation.Result{}, nil
		}), nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"text":"report","session":"other"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown input status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestNonLoopbackAddressNeedsToken(t *testing.T) {
	if isLoopback("0.0.0.0:8080") {
		t.Fatal("0.0.0.0 must be treated as a network listener")
	}
	if !isLoopback("127.0.0.1:8080") {
		t.Fatal("127.0.0.1 must be treated as loopback")
	}
}

func TestLoadEnvironmentReadsValuesWithoutExportingThem(t *testing.T) {
	t.Setenv("CLAUDE_OAUTH_TOKEN", "")
	directory := t.TempDir()
	contents := "OTHER_VALUE=ignored\nexport CLAUDE_OAUTH_TOKEN='test-oauth-token'\n"
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write environment file: %v", err)
	}

	lookup, err := loadEnvironment("", directory)
	if err != nil {
		t.Fatalf("load environment: %v", err)
	}
	got := []string{lookup("CLAUDE_OAUTH_TOKEN"), lookup("OTHER_VALUE")}
	want := []string{"test-oauth-token", "ignored"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment lookup did not return expected values")
	}
}

func TestLoadEnvironmentUsesProcessEnvironmentFirst(t *testing.T) {
	t.Setenv("CLAUDE_OAUTH_TOKEN", "process-token")
	directory := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(directory, ".env"), []byte("CLAUDE_OAUTH_TOKEN=file-token\n"), 0o600,
	); err != nil {
		t.Fatalf("write environment file: %v", err)
	}

	lookup, err := loadEnvironment("", directory)
	if err != nil {
		t.Fatalf("load environment: %v", err)
	}
	if got := lookup("CLAUDE_OAUTH_TOKEN"); got != "process-token" {
		t.Fatalf("OAuth token = %q, want process value", got)
	}
}
