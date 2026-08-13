package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) QueueMessageRun(ctx context.Context, run MessageRun) (MessageRun, bool, error) {
	if run.ID == "" || run.MessageID == "" || run.Scope.Key == "" ||
		run.Scope.RoomID == "" || run.Text == "" || run.Mode == "" {
		return MessageRun{}, false, errors.New("message run needs IDs, scope, text, and mode")
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO message_runs (
  run_id, message_id, scope_key, room_id, root_message_id, sender_id,
  text, mode, persona, status, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?)`,
		run.ID, run.MessageID, run.Scope.Key, run.Scope.RoomID,
		run.Scope.RootMessageID, run.SenderID, run.Text, run.Mode, run.Persona,
		run.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return MessageRun{}, false, fmt.Errorf("queue message run: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return MessageRun{}, false, fmt.Errorf("read queue result: %w", err)
	}
	stored, found, err := s.messageRunBy(ctx, "message_id", run.MessageID)
	if err != nil || !found {
		return MessageRun{}, false, err
	}
	return stored, rows == 1, nil
}

func (s *Store) MessageRun(ctx context.Context, runID string) (MessageRun, bool, error) {
	return s.messageRunBy(ctx, "run_id", runID)
}

func (s *Store) messageRunBy(
	ctx context.Context, column, value string,
) (MessageRun, bool, error) {
	query := `
SELECT run_id, message_id, scope_key, room_id, root_message_id, sender_id,
       text, mode, persona, status, result_persona, active_run_id, reply,
       stop_reason, generation, cached, steered, error, created_at,
       started_at, completed_at
FROM message_runs WHERE ` + column + ` = ?`
	var run MessageRun
	var created, started, completed string
	err := s.db.QueryRowContext(ctx, query, value).Scan(
		&run.ID, &run.MessageID, &run.Scope.Key, &run.Scope.RoomID,
		&run.Scope.RootMessageID, &run.SenderID, &run.Text, &run.Mode,
		&run.Persona, &run.Status, &run.ResultPersona, &run.ActiveRunID,
		&run.Reply, &run.StopReason, &run.Generation, &run.Cached, &run.Steered,
		&run.Error, &created, &started, &completed,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageRun{}, false, nil
	}
	if err != nil {
		return MessageRun{}, false, fmt.Errorf("read message run %q: %w", value, err)
	}
	if run.CreatedAt, err = parseStoredTime(created); err != nil {
		return MessageRun{}, false, err
	}
	if started != "" {
		run.StartedAt, err = parseStoredTime(started)
	}
	if err == nil && completed != "" {
		run.CompletedAt, err = parseStoredTime(completed)
	}
	return run, true, err
}

func (s *Store) ClaimMessageRun(ctx context.Context) (MessageRun, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageRun{}, false, fmt.Errorf("begin message run claim: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback is best-effort after commit.
	var runID string
	err = tx.QueryRowContext(ctx, `
SELECT run_id FROM message_runs WHERE status = 'queued'
ORDER BY created_at, run_id LIMIT 1`).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageRun{}, false, nil
	}
	if err != nil {
		return MessageRun{}, false, fmt.Errorf("select message run: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
UPDATE message_runs SET status = 'running', started_at = ?
WHERE run_id = ? AND status = 'queued'`, now, runID)
	if err != nil {
		return MessageRun{}, false, fmt.Errorf("claim message run: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return MessageRun{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return MessageRun{}, false, fmt.Errorf("commit message run claim: %w", err)
	}
	run, found, err := s.MessageRun(ctx, runID)
	return run, found, err
}

func (s *Store) CompleteMessageRun(ctx context.Context, run MessageRun) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE message_runs SET
  status = 'completed', result_persona = ?, active_run_id = ?, reply = ?,
  stop_reason = ?, generation = ?, cached = ?, steered = ?, error = '',
  completed_at = ?
WHERE run_id = ?`, run.ResultPersona, run.ActiveRunID, run.Reply,
		run.StopReason, run.Generation, run.Cached, run.Steered,
		time.Now().UTC().Format(time.RFC3339Nano), run.ID)
	if err != nil {
		return fmt.Errorf("complete message run %q: %w", run.ID, err)
	}
	return nil
}

func (s *Store) FailMessageRun(ctx context.Context, runID string, failure error) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE message_runs SET status = 'failed', error = ?, completed_at = ?
WHERE run_id = ?`, truncateUTF8(failure.Error(), 2000),
		time.Now().UTC().Format(time.RFC3339Nano), runID)
	if err != nil {
		return fmt.Errorf("fail message run %q: %w", runID, err)
	}
	return nil
}

func (s *Store) RequeueMessageRun(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE message_runs SET status = 'queued', started_at = ''
WHERE run_id = ? AND status = 'running'`, runID)
	if err != nil {
		return fmt.Errorf("requeue message run %q: %w", runID, err)
	}
	return nil
}
