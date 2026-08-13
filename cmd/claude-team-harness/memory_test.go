package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryCLIReadsWritesAndSeparatesPersonas(t *testing.T) {
	t.Setenv(memoryDBEnvironment, filepath.Join(t.TempDir(), "memory.db"))
	t.Setenv(memoryPersonaEnv, "")
	notes := filepath.Join(t.TempDir(), "memory.md")
	if err := os.WriteFile(notes, []byte("Remember cedar-17.\n"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"memory", "write", "--as", "Project-Manager", "--file", notes,
	}, &output); err != nil {
		t.Fatalf("memory write: %v", err)
	}
	if body := output.String(); !strings.Contains(body, `"persona":"project-manager"`) ||
		!strings.Contains(body, `"lines":1`) || strings.Contains(body, "cedar-17") {
		t.Fatalf("write receipt = %s", body)
	}

	output.Reset()
	out := filepath.Join(t.TempDir(), "out")
	if err := run(context.Background(), []string{
		"memory", "read", "--as", "project-manager", "--out", out,
	}, &output); err != nil {
		t.Fatalf("memory read: %v", err)
	}
	if !strings.Contains(output.String(), "cedar-17") {
		t.Fatalf("read output = %s", output.String())
	}
	written, err := os.ReadFile(filepath.Join(out, "memory.md"))
	if err != nil || string(written) != "Remember cedar-17.\n" {
		t.Fatalf("output file = %q, %v", written, err)
	}

	output.Reset()
	if err := run(context.Background(), []string{
		"memory", "read", "--as", "engineer",
	}, &output); err != nil || !strings.Contains(output.String(), `"lines":0`) {
		t.Fatalf("engineer read = %s, %v", output.String(), err)
	}
}

func TestMemoryCLIBindsSessionPersona(t *testing.T) {
	t.Setenv(memoryDBEnvironment, filepath.Join(t.TempDir(), "memory.db"))
	t.Setenv(memoryPersonaEnv, "project-manager")
	notes := filepath.Join(t.TempDir(), "memory.md")
	if err := os.WriteFile(notes, []byte("Own memory.\n"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"memory", "write", "--file", notes,
	}, &output); err != nil {
		t.Fatalf("self write: %v", err)
	}
	if err := run(context.Background(), []string{
		"memory", "read", "--as", "engineer",
	}, &output); err == nil || !strings.Contains(err.Error(), "drop --as") {
		t.Fatalf("cross-persona read error = %v", err)
	}
}

func TestMemoryCLIRequiresIdentityAndFile(t *testing.T) {
	t.Setenv(memoryDBEnvironment, filepath.Join(t.TempDir(), "memory.db"))
	t.Setenv(memoryPersonaEnv, "")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"memory", "read"}, &output); err == nil ||
		!strings.Contains(err.Error(), "needs --as") {
		t.Fatalf("missing identity error = %v", err)
	}
	if err := run(context.Background(), []string{
		"memory", "write", "--as", "project-manager",
	}, &output); err == nil || !strings.Contains(err.Error(), "needs --file") {
		t.Fatalf("missing file error = %v", err)
	}
}
