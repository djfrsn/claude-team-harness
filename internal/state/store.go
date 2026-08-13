// Package state stores durable conversation and Webex delivery state.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state database path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate() error {
	const schema = `
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS conversations (
  scope_key TEXT PRIMARY KEY,
  room_id TEXT NOT NULL,
  root_message_id TEXT NOT NULL DEFAULT '',
  acp_session_id TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 0,
  context_epoch INTEGER NOT NULL DEFAULT 1,
  completed_turns INTEGER NOT NULL DEFAULT 0,
  handoff TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
  message_id TEXT PRIMARY KEY,
  scope_key TEXT NOT NULL,
  room_id TEXT NOT NULL,
  root_message_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
  sender_id TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL,
  in_reply_to TEXT NOT NULL DEFAULT '',
  context_epoch INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_scope_created
  ON messages(scope_key, created_at, message_id);
CREATE INDEX IF NOT EXISTS messages_reply
  ON messages(in_reply_to, role);
CREATE TABLE IF NOT EXISTS webex_events (
  message_id TEXT PRIMARY KEY,
  room_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'done', 'failed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS routes (
  scope_key TEXT PRIMARY KEY,
  persona TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
  run_id TEXT PRIMARY KEY,
  scope_key TEXT NOT NULL,
  persona TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
  session_id TEXT NOT NULL DEFAULT '',
  reply TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS runtime_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id TEXT NOT NULL,
  persona TEXT NOT NULL,
  kind TEXT NOT NULL,
  session_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate state database: %w", err)
	}
	_, err := s.db.Exec(`UPDATE webex_events SET status = 'pending' WHERE status = 'processing'`)
	if err != nil {
		return fmt.Errorf("recover Webex events: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Conversation(ctx context.Context, scope Scope) (Conversation, bool, error) {
	var conversation Conversation
	var updated string
	err := s.db.QueryRowContext(ctx, `
SELECT room_id, root_message_id, acp_session_id, generation, context_epoch,
       completed_turns, handoff, updated_at
FROM conversations WHERE scope_key = ?`, scope.Key).Scan(
		&conversation.Scope.RoomID, &conversation.Scope.RootMessageID,
		&conversation.ACPSessionID, &conversation.Generation,
		&conversation.ContextEpoch,
		&conversation.CompletedTurns, &conversation.Handoff, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, false, nil
	}
	if err != nil {
		return Conversation{}, false, fmt.Errorf("read conversation %q: %w", scope.Key, err)
	}
	conversation.Scope.Key = scope.Key
	conversation.UpdatedAt, err = parseStoredTime(updated)
	if err != nil {
		return Conversation{}, false, fmt.Errorf("parse conversation %q update time: %w", scope.Key, err)
	}
	return conversation, true, nil
}

func (s *Store) PutConversation(ctx context.Context, conversation Conversation) error {
	if conversation.Scope.Key == "" || conversation.Scope.RoomID == "" {
		return errors.New("conversation needs a scope key and room ID")
	}
	now := conversation.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if conversation.ContextEpoch <= 0 {
		conversation.ContextEpoch = 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO conversations (
  scope_key, room_id, root_message_id, acp_session_id, generation, context_epoch,
  completed_turns, handoff, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(scope_key) DO UPDATE SET
  room_id = excluded.room_id,
  root_message_id = excluded.root_message_id,
  acp_session_id = excluded.acp_session_id,
  generation = excluded.generation,
  context_epoch = excluded.context_epoch,
  completed_turns = excluded.completed_turns,
  handoff = excluded.handoff,
  updated_at = excluded.updated_at`,
		conversation.Scope.Key, conversation.Scope.RoomID,
		conversation.Scope.RootMessageID, conversation.ACPSessionID,
		conversation.Generation, conversation.ContextEpoch, conversation.CompletedTurns,
		conversation.Handoff, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("write conversation %q: %w", conversation.Scope.Key, err)
	}
	return nil
}

func (s *Store) AddMessage(ctx context.Context, message Message) (bool, error) {
	if message.ID == "" || message.Scope.Key == "" || message.Scope.RoomID == "" {
		return false, errors.New("message needs an ID, scope key, and room ID")
	}
	if message.Role != "user" && message.Role != "assistant" {
		return false, fmt.Errorf("message role must be user or assistant, got %q", message.Role)
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	if message.ContextEpoch <= 0 {
		message.ContextEpoch = 1
	}
	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO messages (
  message_id, scope_key, room_id, root_message_id, role, sender_id,
  text, in_reply_to, context_epoch, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.Scope.Key, message.Scope.RoomID,
		message.Scope.RootMessageID, message.Role, message.SenderID,
		message.Text, message.InReplyTo, message.ContextEpoch,
		message.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, fmt.Errorf("write message %q: %w", message.ID, err)
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *Store) ReplyTo(ctx context.Context, messageID string) (string, bool, error) {
	var reply string
	err := s.db.QueryRowContext(ctx, `
SELECT text FROM messages
WHERE in_reply_to = ? AND role = 'assistant'
ORDER BY created_at DESC LIMIT 1`, messageID).Scan(&reply)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read reply to %q: %w", messageID, err)
	}
	return reply, true, nil
}

func (s *Store) Message(ctx context.Context, messageID string) (Message, bool, error) {
	var message Message
	var created string
	err := s.db.QueryRowContext(ctx, `
SELECT scope_key, room_id, root_message_id, role, sender_id, text,
       in_reply_to, context_epoch, created_at
FROM messages WHERE message_id = ?`, messageID).Scan(
		&message.Scope.Key, &message.Scope.RoomID, &message.Scope.RootMessageID,
		&message.Role, &message.SenderID, &message.Text, &message.InReplyTo,
		&message.ContextEpoch, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("read message %q: %w", messageID, err)
	}
	message.ID = messageID
	message.CreatedAt, err = parseStoredTime(created)
	if err != nil {
		return Message{}, false, fmt.Errorf("parse message %q creation time: %w", messageID, err)
	}
	return message, true, nil
}

// BuildHandoff returns recent scope messages. A new thread also receives its
// root message and the room agent's response to that message.
func (s *Store) BuildHandoff(
	ctx context.Context, scope Scope, excludeMessageID string, contextEpoch int,
	includeThreadRoot bool, maxMessages, maxChars int,
) (string, error) {
	if maxMessages <= 0 || maxChars <= 0 {
		return "", nil
	}
	messages := make([]Message, 0, maxMessages+2)
	seen := make(map[string]bool)
	if scope.RootMessageID != "" && includeThreadRoot {
		root, ok, err := s.Message(ctx, scope.RootMessageID)
		if err != nil {
			return "", err
		}
		if ok && root.ID != excludeMessageID {
			messages = append(messages, root)
			seen[root.ID] = true
		}
		rows, err := s.db.QueryContext(ctx, `
SELECT message_id, scope_key, room_id, root_message_id, role, sender_id,
       text, in_reply_to, context_epoch, created_at
FROM messages
WHERE in_reply_to = ? AND role = 'assistant'
ORDER BY created_at LIMIT 1`, scope.RootMessageID)
		if err != nil {
			return "", fmt.Errorf("read root response: %w", err)
		}
		rootReplies, err := scanMessages(rows)
		if err != nil {
			return "", err
		}
		for _, message := range rootReplies {
			if message.ID != excludeMessageID {
				messages = append(messages, message)
				seen[message.ID] = true
			}
		}
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT message_id, scope_key, room_id, root_message_id, role, sender_id,
       text, in_reply_to, context_epoch, created_at
FROM messages WHERE scope_key = ? AND message_id != ? AND context_epoch = ?
ORDER BY created_at DESC, message_id DESC LIMIT ?`,
		scope.Key, excludeMessageID, contextEpoch, maxMessages)
	if err != nil {
		return "", fmt.Errorf("read handoff messages: %w", err)
	}
	recent, err := scanMessages(rows)
	if err != nil {
		return "", err
	}
	for left, right := 0, len(recent)-1; left < right; left, right = left+1, right-1 {
		recent[left], recent[right] = recent[right], recent[left]
	}
	for _, message := range recent {
		if !seen[message.ID] {
			messages = append(messages, message)
		}
	}

	var handoff strings.Builder
	for _, message := range messages {
		label := "Human"
		if message.Role == "assistant" {
			label = "Agent"
		}
		line := fmt.Sprintf("%s: %s\n", label, strings.TrimSpace(message.Text))
		if handoff.Len()+len(line) > maxChars {
			remaining := maxChars - handoff.Len()
			if remaining > 0 {
				handoff.WriteString(truncateUTF8(line, remaining))
			}
			break
		}
		handoff.WriteString(line)
	}
	return strings.TrimSpace(handoff.String()), nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		var created string
		if err := rows.Scan(
			&message.ID, &message.Scope.Key, &message.Scope.RoomID,
			&message.Scope.RootMessageID, &message.Role, &message.SenderID,
			&message.Text, &message.InReplyTo, &message.ContextEpoch, &created,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		var err error
		message.CreatedAt, err = parseStoredTime(created)
		if err != nil {
			return nil, fmt.Errorf("parse message %q creation time: %w", message.ID, err)
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) EnqueueWebex(ctx context.Context, event WebexEvent) (bool, error) {
	if event.MessageID == "" || event.RoomID == "" {
		return false, errors.New("Webex event needs a message ID and room ID")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	now := event.CreatedAt.Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO webex_events (
  message_id, room_id, status, attempts, next_attempt_at, created_at, updated_at
) VALUES (?, ?, 'pending', 0, ?, ?, ?)`, event.MessageID, event.RoomID, now, now, now)
	if err != nil {
		return false, fmt.Errorf("enqueue Webex event: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *Store) ClaimWebex(ctx context.Context, now time.Time) (WebexEvent, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebexEvent{}, false, fmt.Errorf("begin Webex claim: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback is best-effort after a successful commit.
	var event WebexEvent
	var created string
	err = tx.QueryRowContext(ctx, `
SELECT message_id, room_id, attempts, created_at
FROM webex_events
WHERE status = 'pending' AND next_attempt_at <= ?
ORDER BY created_at LIMIT 1`, now.UTC().Format(time.RFC3339Nano)).Scan(
		&event.MessageID, &event.RoomID, &event.Attempts, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WebexEvent{}, false, nil
	}
	if err != nil {
		return WebexEvent{}, false, fmt.Errorf("select Webex event: %w", err)
	}
	event.CreatedAt, err = parseStoredTime(created)
	if err != nil {
		return WebexEvent{}, false, fmt.Errorf("parse Webex event %q creation time: %w", event.MessageID, err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE webex_events SET status = 'processing', updated_at = ?
WHERE message_id = ? AND status = 'pending'`,
		now.UTC().Format(time.RFC3339Nano), event.MessageID)
	if err != nil {
		return WebexEvent{}, false, fmt.Errorf("claim Webex event: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return WebexEvent{}, false, fmt.Errorf("read Webex claim result: %w", err)
	}
	if rows != 1 {
		return WebexEvent{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return WebexEvent{}, false, fmt.Errorf("commit Webex claim: %w", err)
	}
	return event, true, nil
}

func parseStoredTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid stored timestamp: %w", err)
	}
	return parsed, nil
}

func (s *Store) CompleteWebex(ctx context.Context, messageID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE webex_events SET status = 'done', updated_at = ?, last_error = ''
WHERE message_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), messageID)
	if err != nil {
		return fmt.Errorf("complete Webex event: %w", err)
	}
	return nil
}

func (s *Store) RetryWebex(ctx context.Context, event WebexEvent, failure error) error {
	attempts := event.Attempts + 1
	status := "pending"
	if attempts >= 5 {
		status = "failed"
	}
	delay := time.Second << min(attempts, 6)
	now := time.Now().UTC()
	failureText := truncateUTF8(failure.Error(), 1000)
	_, err := s.db.ExecContext(ctx, `
UPDATE webex_events
SET status = ?, attempts = ?, next_attempt_at = ?, last_error = ?, updated_at = ?
	WHERE message_id = ?`, status, attempts, now.Add(delay).Format(time.RFC3339Nano),
		failureText, now.Format(time.RFC3339Nano), event.MessageID)
	if err != nil {
		return fmt.Errorf("retry Webex event: %w", err)
	}
	return nil
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	for maxBytes > 0 && !utf8.ValidString(value[:maxBytes]) {
		maxBytes--
	}
	return value[:maxBytes]
}
