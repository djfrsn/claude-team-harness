package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestConnectionLossReportsAdapterUnavailable(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	connection := newConn(clientInput, io.Discard, func(string, json.RawMessage, responder) {})
	response, err := connection.call("session/prompt", map[string]string{"sessionId": "lost"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := agentOutput.Close(); err != nil {
		t.Fatalf("close adapter output: %v", err)
	}
	if result := <-response; !errors.Is(result.err, ErrAdapterUnavailable) {
		t.Fatalf("pending call error = %v, want adapter unavailable", result.err)
	}
	connection.wg.Wait()
	client := &Client{conn: connection, done: make(chan struct{})}
	if !client.Unavailable() {
		t.Fatal("client with a closed connection reports available")
	}
	if _, err := connection.call("session/new", nil); !errors.Is(err, ErrAdapterUnavailable) {
		t.Fatalf("closed connection error = %v, want adapter unavailable", err)
	}
}

func TestPromptReturnsStreamedAgentMessage(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	agentInput, clientOutput := io.Pipe()
	client := &Client{policy: PermissionDeny, done: make(chan struct{})}
	client.conn = newConn(clientInput, clientOutput, client.serve)
	t.Cleanup(func() {
		_ = agentOutput.Close()
		_ = clientOutput.Close()
	})

	go func() {
		decoder := json.NewDecoder(agentInput)
		for {
			var request message
			if decoder.Decode(&request) != nil {
				return
			}
			switch request.Method {
			case "session/new":
				sendTestMessage(t, agentOutput, message{
					JSONRPC: "2.0", ID: request.ID, Result: marshal(map[string]string{"sessionId": "session-1"}),
				})
			case "session/prompt":
				sendTestMessage(t, agentOutput, message{
					JSONRPC: "2.0", Method: "session/update", Params: marshal(map[string]any{
						"sessionId": "session-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]string{"type": "text", "text": "on track"},
						},
					}),
				})
				sendTestMessage(t, agentOutput, message{
					JSONRPC: "2.0", ID: request.ID, Result: marshal(map[string]StopReason{"stopReason": StopEndTurn}),
				})
			}
		}
	}()

	sessionID, err := client.NewSession(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	turn, err := client.Prompt(context.Background(), sessionID, "report")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if turn.Reply != "on track" || turn.StopReason != StopEndTurn {
		t.Fatalf("Prompt returned %+v, want reply on track and stop reason end_turn", turn)
	}
}

func TestPermissionPolicySelectsOneTimeOption(t *testing.T) {
	params := marshal(map[string]any{"options": []map[string]string{
		{"kind": "allow_once", "optionId": "allow"},
		{"kind": "reject_once", "optionId": "reject"},
	}})
	for _, test := range []struct {
		name   string
		policy PermissionPolicy
		want   string
	}{
		{name: "deny", policy: PermissionDeny, want: "reject"},
		{name: "allow once", policy: PermissionAllowOnce, want: "allow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{policy: test.policy}
			result, rpcErr := client.permission(params)
			if rpcErr != nil {
				t.Fatalf("permission: %v", rpcErr)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal permission result: %v", err)
			}
			var selected struct {
				Outcome struct {
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			}
			if err := json.Unmarshal(data, &selected); err != nil {
				t.Fatalf("decode permission result: %v", err)
			}
			if selected.Outcome.OptionID != test.want {
				t.Fatalf("permission selected %q, want %q", selected.Outcome.OptionID, test.want)
			}
		})
	}
}

func TestLoadSessionCarriesSavedSession(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	agentInput, clientOutput := io.Pipe()
	client := &Client{policy: PermissionDeny, done: make(chan struct{}), logf: t.Logf}
	client.conn = newConn(clientInput, clientOutput, client.serve)
	t.Cleanup(func() {
		_ = agentOutput.Close()
		_ = clientOutput.Close()
	})

	params := make(chan json.RawMessage, 1)
	go func() {
		decoder := json.NewDecoder(agentInput)
		var request message
		if decoder.Decode(&request) != nil {
			return
		}
		params <- request.Params
		sendTestMessage(t, agentOutput, message{
			JSONRPC: "2.0", ID: request.ID,
			Result: marshal(map[string]string{"sessionId": "saved-session"}),
		})
	}()
	if err := client.LoadSession(context.Background(), t.TempDir(), "saved-session", nil); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	var got struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(<-params, &got); err != nil || got.SessionID != "saved-session" {
		t.Fatalf("session/load params = %+v, err = %v", got, err)
	}
}

func TestSteerInjectsIntoActiveSession(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	agentInput, clientOutput := io.Pipe()
	client := &Client{
		policy: PermissionDeny, done: make(chan struct{}), logf: t.Logf, steering: true,
	}
	client.conn = newConn(clientInput, clientOutput, client.serve)
	t.Cleanup(func() {
		_ = agentOutput.Close()
		_ = clientOutput.Close()
	})

	params := make(chan json.RawMessage, 1)
	go func() {
		decoder := json.NewDecoder(agentInput)
		var request message
		if decoder.Decode(&request) != nil {
			return
		}
		if request.Method != "_session/steering" {
			t.Errorf("method = %q, want _session/steering", request.Method)
		}
		params <- request.Params
		sendTestMessage(t, agentOutput, message{
			JSONRPC: "2.0", ID: request.ID,
			Result: marshal(map[string]SteerOutcome{"outcome": SteerInjected}),
		})
	}()
	outcome, err := client.Steer(context.Background(), "session-1", "Use Friday")
	if err != nil || outcome != SteerInjected {
		t.Fatalf("Steer = (%q, %v), want injected", outcome, err)
	}
	var got struct {
		SessionID string `json:"sessionId"`
		Meta      struct {
			Steering struct {
				IdleBehavior string `json:"idleBehavior"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(<-params, &got); err != nil {
		t.Fatalf("decode steer params: %v", err)
	}
	if got.SessionID != "session-1" || got.Meta.Steering.IdleBehavior != "promptRequired" {
		t.Fatalf("steer params = %+v", got)
	}
}

func sendTestMessage(t *testing.T, writer io.Writer, value message) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Errorf("marshal test message: %v", err)
		return
	}
	if _, err := writer.Write(append(data, '\n')); err != nil {
		t.Logf("write test message: %v", err)
	}
}
