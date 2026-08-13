package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) Route(ctx context.Context, scopeKey string) (string, error) {
	var persona string
	err := s.db.QueryRowContext(ctx, `SELECT persona FROM routes WHERE scope_key = ?`, scopeKey).
		Scan(&persona)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read route %q: %w", scopeKey, err)
	}
	return persona, nil
}

func (s *Store) PutRoute(ctx context.Context, scopeKey, persona string) error {
	if scopeKey == "" || persona == "" {
		return errors.New("route needs a scope key and persona")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO routes (scope_key, persona, updated_at) VALUES (?, ?, ?)
ON CONFLICT(scope_key) DO UPDATE SET persona = excluded.persona, updated_at = excluded.updated_at`,
		scopeKey, persona, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write route %q: %w", scopeKey, err)
	}
	return nil
}

type Run struct {
	ID          string
	ScopeKey    string
	Persona     string
	Status      string
	SessionID   string
	Reply       string
	Error       string
	StartedAt   time.Time
	CompletedAt time.Time
}

func (s *Store) StartRun(ctx context.Context, run Run) error {
	if run.ID == "" || run.ScopeKey == "" || run.Persona == "" {
		return errors.New("run needs an ID, scope key, and persona")
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO runs (run_id, scope_key, persona, status, started_at)
VALUES (?, ?, ?, 'running', ?)`, run.ID, run.ScopeKey, run.Persona,
		run.StartedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("start run %q: %w", run.ID, err)
	}
	return nil
}

func (s *Store) FinishRun(
	ctx context.Context, runID, sessionID, reply string, runErr error,
) error {
	status := "completed"
	errorText := ""
	if runErr != nil {
		status = "failed"
		errorText = runErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE runs SET status = ?, session_id = ?, reply = ?, error = ?, completed_at = ?
WHERE run_id = ?`, status, sessionID, reply, errorText,
		time.Now().UTC().Format(time.RFC3339Nano), runID)
	if err != nil {
		return fmt.Errorf("finish run %q: %w", runID, err)
	}
	return nil
}

func (s *Store) AddRuntimeEvent(
	ctx context.Context, runID, persona, kind, sessionID, eventState string,
) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO runtime_events (run_id, persona, kind, session_id, state, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, runID, persona, kind, sessionID, eventState,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write runtime event: %w", err)
	}
	return nil
}
