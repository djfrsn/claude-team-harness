package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersonaDocumentsAreBoundedAndSeparate(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)

	empty, err := store.Read(ctx, "Project-Manager")
	if err != nil || empty.Body != "" || empty.Lines != 0 || empty.UpdatedAt != nil {
		t.Fatalf("empty read = (%+v, %v)", empty, err)
	}
	body := "# Delivery\n\n- Friday is the review day.\n"
	written, err := store.Write(ctx, now, "Project-Manager", body)
	if err != nil || written.Persona != "project-manager" || written.Lines != 3 {
		t.Fatalf("write = (%+v, %v)", written, err)
	}
	read, err := store.Read(ctx, "project-manager")
	if err != nil || read.Body != body || read.UpdatedAt == nil || !read.UpdatedAt.Equal(now) {
		t.Fatalf("read = (%+v, %v)", read, err)
	}
	other, err := store.Read(ctx, "engineer")
	if err != nil || other.Body != "" {
		t.Fatalf("other persona read = (%+v, %v)", other, err)
	}

	if _, err := store.Write(ctx, now, "project-manager",
		strings.Repeat("line\n", MaxLines+1)); err == nil {
		t.Fatalf("write over %d lines succeeded", MaxLines)
	}
	unchanged, err := store.Read(ctx, "project-manager")
	if err != nil || unchanged.Body != body {
		t.Fatalf("rejected write changed memory = (%+v, %v)", unchanged, err)
	}
}

func TestWriteReplacesDocumentAndCountLines(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, body := range []string{"first\n", "second\n"} {
		if _, err := store.Write(ctx, time.Now(), "planner", body); err != nil {
			t.Fatalf("write %q: %v", body, err)
		}
	}
	doc, err := store.Read(ctx, "planner")
	if err != nil || doc.Body != "second\n" {
		t.Fatalf("replacement = (%+v, %v)", doc, err)
	}
	for body, want := range map[string]int{
		"": 0, "one": 1, "one\n": 1, "one\ntwo": 2, "one\ntwo\n": 2,
	} {
		if got := CountLines(body); got != want {
			t.Errorf("CountLines(%q) = %d, want %d", body, got, want)
		}
	}
}

func TestMemoryRejectsMissingIdentityAndPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open accepted an empty path")
	}
	store := openTestStore(t)
	if _, err := store.Read(context.Background(), "  "); err == nil {
		t.Fatal("Read accepted an empty persona")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
