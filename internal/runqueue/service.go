// Package runqueue executes durable transport-neutral message requests.
package runqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/state"
)

type Handler interface {
	Handle(context.Context, conversation.Input) (conversation.Result, error)
}

type Config struct {
	Store       *state.Store
	Handler     Handler
	Workers     int
	TurnTimeout time.Duration
	Validate    func(string) error
	Logf        func(string, ...any)
}

type Service struct {
	cfg  Config
	wake chan struct{}
	done chan struct{}
}

type ConflictError struct {
	MessageID string
}

func (e ConflictError) Error() string {
	return fmt.Sprintf("message ID %q already has different content", e.MessageID)
}

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Handler == nil || cfg.TurnTimeout <= 0 {
		return nil, errors.New("run queue needs a store, handler, and positive turn timeout")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	service := &Service{cfg: cfg, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go service.run(ctx)
	return service, nil
}

func (s *Service) Submit(
	ctx context.Context, input conversation.Input,
) (state.MessageRun, bool, error) {
	input.Text = strings.TrimSpace(input.Text)
	if input.Scope.Key == "" || input.Scope.RoomID == "" || input.Text == "" {
		return state.MessageRun{}, false, errors.New("message needs a scope and text")
	}
	mode, err := conversation.ParseMode(string(input.Mode))
	if err != nil {
		return state.MessageRun{}, false, err
	}
	if s.cfg.Validate != nil {
		if err := s.cfg.Validate(input.Persona); err != nil {
			return state.MessageRun{}, false, err
		}
	}
	if input.MessageID == "" {
		input.MessageID, err = randomID("message")
		if err != nil {
			return state.MessageRun{}, false, err
		}
	}
	runID, err := randomID("run")
	if err != nil {
		return state.MessageRun{}, false, err
	}
	wanted := state.MessageRun{
		ID: runID, Scope: input.Scope, MessageID: input.MessageID,
		SenderID: input.SenderID, Text: input.Text, Mode: string(mode),
		Persona: input.Persona, CreatedAt: input.CreatedAt,
	}
	stored, created, err := s.cfg.Store.QueueMessageRun(ctx, wanted)
	if err != nil {
		return state.MessageRun{}, false, err
	}
	if !created && !sameRequest(stored, wanted) {
		return state.MessageRun{}, false, ConflictError{MessageID: input.MessageID}
	}
	if created {
		s.Wake()
	}
	return stored, created, nil
}

func (s *Service) Run(ctx context.Context, runID string) (state.MessageRun, bool, error) {
	return s.cfg.Store.MessageRun(ctx, runID)
}

func (s *Service) Wait(ctx context.Context, runID string) (state.MessageRun, bool, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, found, err := s.Run(ctx, runID)
		if err != nil || !found || terminal(run.Status) {
			return run, found, err
		}
		select {
		case <-ctx.Done():
			return run, true, nil
		case <-ticker.C:
		}
	}
}

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Done() <-chan struct{} { return s.done }

func (s *Service) run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(s.cfg.Workers)
	for range s.cfg.Workers {
		go func() {
			defer workers.Done()
			s.worker(ctx)
		}()
	}
	s.Wake()
	workers.Wait()
	close(s.done)
}

func (s *Service) worker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
		for s.processNext(ctx) {
		}
	}
}

func (s *Service) processNext(ctx context.Context) bool {
	run, found, err := s.cfg.Store.ClaimMessageRun(ctx)
	if err != nil {
		s.cfg.Logf("claim message run: %v", err)
		return false
	}
	if !found {
		return false
	}
	turnCtx, cancel := context.WithTimeout(ctx, s.cfg.TurnTimeout)
	result, turnErr := s.cfg.Handler.Handle(turnCtx, conversation.Input{
		Scope: run.Scope, RunID: run.ID, MessageID: run.MessageID,
		SenderID: run.SenderID, Text: run.Text, Mode: conversation.Mode(run.Mode),
		Persona: run.Persona, CreatedAt: run.CreatedAt,
	})
	cancel()
	if turnErr != nil {
		if ctx.Err() != nil {
			if err := s.cfg.Store.RequeueMessageRun(context.WithoutCancel(ctx), run.ID); err != nil {
				s.cfg.Logf("requeue interrupted message run %s: %v", run.ID, err)
			}
			return true
		}
		if err := s.cfg.Store.FailMessageRun(context.WithoutCancel(ctx), run.ID, turnErr); err != nil {
			s.cfg.Logf("fail message run %s: %v", run.ID, err)
		}
		return true
	}
	run.ResultPersona = result.Persona
	if result.RunID != run.ID {
		run.ActiveRunID = result.RunID
	}
	run.Reply = result.Reply
	run.StopReason = string(result.StopReason)
	run.Generation = result.Generation
	run.Cached = result.Cached
	run.Steered = result.Steered
	if err := s.cfg.Store.CompleteMessageRun(context.WithoutCancel(ctx), run); err != nil {
		s.cfg.Logf("complete message run %s: %v", run.ID, err)
	}
	return true
}

func sameRequest(left, right state.MessageRun) bool {
	return left.Scope == right.Scope && left.SenderID == right.SenderID &&
		left.Text == right.Text && left.Mode == right.Mode && left.Persona == right.Persona
}

func terminal(status string) bool { return status == "completed" || status == "failed" }

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create %s ID: %w", prefix, err)
	}
	return prefix + ":" + hex.EncodeToString(value[:]), nil
}
