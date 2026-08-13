package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestPromptReturnsWhenAdapterIgnoresCancellation(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	agentInput, clientOutput := io.Pipe()
	client := &Client{
		policy: PermissionDeny, done: make(chan struct{}), cancelGrace: 25 * time.Millisecond,
	}
	client.conn = newConn(clientInput, clientOutput, client.serve)
	t.Cleanup(func() {
		_ = agentOutput.Close()
		_ = clientOutput.Close()
	})

	promptStarted := make(chan struct{})
	cancelReceived := make(chan struct{})
	go func() {
		decoder := json.NewDecoder(agentInput)
		var request message
		if decoder.Decode(&request) != nil {
			return
		}
		if request.Method != "session/prompt" {
			t.Errorf("method = %q, want session/prompt", request.Method)
		}
		close(promptStarted)
		if decoder.Decode(&request) != nil {
			return
		}
		if request.Method != "session/cancel" {
			t.Errorf("method = %q, want session/cancel", request.Method)
		}
		close(cancelReceived)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Prompt(ctx, "session-1", "report")
		result <- err
	}()
	<-promptStarted
	started := time.Now()
	cancel()

	select {
	case <-cancelReceived:
	case <-time.After(time.Second):
		t.Fatal("adapter did not receive session/cancel")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt error = %v, want context canceled", err)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("Prompt returned after %s, want at most 250ms", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("Prompt remained blocked after cancellation grace")
	}
	if _, err := client.Prompt(context.Background(), "session-1", "next report"); !errors.Is(err, errAdapterRetired) {
		t.Fatalf("second Prompt error = %v, want retired adapter", err)
	}
}

func TestPromptCancellationGraceBoundsBlockedCancelWrite(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	writer := &blockingCancelWriter{promptWritten: make(chan struct{}), unblock: make(chan struct{})}
	client := &Client{
		policy: PermissionDeny, done: make(chan struct{}), cancelGrace: 25 * time.Millisecond,
	}
	client.conn = newConn(clientInput, writer, client.serve)
	t.Cleanup(func() {
		close(writer.unblock)
		_ = agentOutput.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Prompt(ctx, "session-1", "report")
		result <- err
	}()
	<-writer.promptWritten
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prompt remained blocked on session/cancel write")
	}
}

type blockingCancelWriter struct {
	mu            sync.Mutex
	writes        int
	promptWritten chan struct{}
	unblock       chan struct{}
}

func (w *blockingCancelWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	writeNumber := w.writes
	w.mu.Unlock()
	if writeNumber == 1 {
		close(w.promptWritten)
		return len(data), nil
	}
	<-w.unblock
	return len(data), nil
}

func TestPromptKeepsResponseReceivedDuringCancellationGrace(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	agentInput, clientOutput := io.Pipe()
	client := &Client{
		policy: PermissionDeny, done: make(chan struct{}), cancelGrace: time.Second,
	}
	client.conn = newConn(clientInput, clientOutput, client.serve)
	t.Cleanup(func() {
		_ = agentOutput.Close()
		_ = clientOutput.Close()
	})

	promptStarted := make(chan struct{})
	go func() {
		decoder := json.NewDecoder(agentInput)
		var prompt message
		if decoder.Decode(&prompt) != nil {
			return
		}
		close(promptStarted)
		var cancel message
		if decoder.Decode(&cancel) != nil {
			return
		}
		if cancel.Method != "session/cancel" {
			t.Errorf("method = %q, want session/cancel", cancel.Method)
		}
		sendTestMessage(t, agentOutput, message{
			JSONRPC: "2.0", Method: "session/update", Params: marshal(map[string]any{
				"sessionId": "session-1",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]string{"type": "text", "text": "finished"},
				},
			}),
		})
		sendTestMessage(t, agentOutput, message{
			JSONRPC: "2.0", ID: prompt.ID,
			Result: marshal(map[string]StopReason{"stopReason": StopEndTurn}),
		})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		turn Turn
		err  error
	}, 1)
	go func() {
		turn, err := client.Prompt(ctx, "session-1", "report")
		result <- struct {
			turn Turn
			err  error
		}{turn: turn, err: err}
	}()
	<-promptStarted
	cancel()

	got := <-result
	if got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	if got.turn.Reply != "finished" || got.turn.StopReason != StopEndTurn {
		t.Fatalf("Prompt returned %+v, want finished and end_turn", got.turn)
	}
}

func TestPromptReturnsContextErrorWhenAdapterAcknowledgesCancellation(t *testing.T) {
	clientInput, agentOutput := io.Pipe()
	agentInput, clientOutput := io.Pipe()
	client := &Client{
		policy: PermissionDeny, done: make(chan struct{}), cancelGrace: time.Second,
	}
	client.conn = newConn(clientInput, clientOutput, client.serve)
	t.Cleanup(func() {
		_ = agentOutput.Close()
		_ = clientOutput.Close()
	})

	promptStarted := make(chan struct{})
	go func() {
		decoder := json.NewDecoder(agentInput)
		var prompt message
		if decoder.Decode(&prompt) != nil {
			return
		}
		close(promptStarted)
		var cancel message
		if decoder.Decode(&cancel) != nil {
			return
		}
		sendTestMessage(t, agentOutput, message{
			JSONRPC: "2.0", ID: prompt.ID,
			Result: marshal(map[string]StopReason{"stopReason": StopCancelled}),
		})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Prompt(ctx, "session-1", "report")
		result <- err
	}()
	<-promptStarted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt error = %v, want context canceled", err)
	}
}
