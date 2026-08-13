// Package memory holds one bounded Markdown memory document per persona.
package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	MaxLines   = 500
	PruneLines = 450
)

const schema = `
CREATE TABLE IF NOT EXISTS memories (
  persona    TEXT PRIMARY KEY,
  body       TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);`

type Document struct {
	Persona   string     `json:"persona"`
	Lines     int        `json:"lines"`
	Body      string     `json:"body"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create memory directory: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open memory database: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply memory schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Read(ctx context.Context, persona string) (Document, error) {
	key, err := normalize(persona)
	if err != nil {
		return Document{}, err
	}
	doc := Document{Persona: key}
	var updatedAt int64
	err = s.db.QueryRowContext(ctx,
		`SELECT body, updated_at FROM memories WHERE persona = ?`, key).
		Scan(&doc.Body, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return doc, nil
	}
	if err != nil {
		return Document{}, fmt.Errorf("read memory for %s: %w", key, err)
	}
	doc.Lines = CountLines(doc.Body)
	updated := time.UnixMilli(updatedAt).UTC()
	doc.UpdatedAt = &updated
	return doc, nil
}

func (s *Store) Write(
	ctx context.Context, now time.Time, persona, body string,
) (Document, error) {
	key, err := normalize(persona)
	if err != nil {
		return Document{}, err
	}
	lines := CountLines(body)
	if lines > MaxLines {
		return Document{}, fmt.Errorf(
			"memory for %s is %d lines and the cap is %d: prune it and write again",
			key, lines, MaxLines,
		)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (persona, body, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(persona) DO UPDATE SET
		  body = excluded.body, updated_at = excluded.updated_at`,
		key, body, now.UnixMilli()); err != nil {
		return Document{}, fmt.Errorf("write memory for %s: %w", key, err)
	}
	updated := now.UTC()
	return Document{
		Persona: key, Lines: lines, Body: body, UpdatedAt: &updated,
	}, nil
}

func CountLines(body string) int {
	if body == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(body, "\n"), "\n") + 1
}

func normalize(persona string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(persona))
	if key == "" {
		return "", errors.New("memory needs a persona name")
	}
	return key, nil
}
