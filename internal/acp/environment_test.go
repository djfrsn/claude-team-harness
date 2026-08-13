package acp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartRestrictsAdapterEnvironment(t *testing.T) {
	directory := t.TempDir()
	environmentFile := filepath.Join(directory, "environment.json")
	originalPath := os.Getenv("PATH")
	t.Setenv("HOME", filepath.Join(directory, "home"))
	t.Setenv("TMPDIR", filepath.Join(directory, "tmp"))
	t.Setenv("WEBEX_BOT_TOKEN", "webex-secret")
	t.Setenv("CLAUDE_TEAM_HARNESS_API_TOKEN", "api-secret")
	t.Setenv("UNRELATED_SECRET", "unrelated-secret")

	client, err := Start(context.Background(), Config{
		Command: `"$ACP_TEST_BINARY" -test.run=^TestACPAdapterProcess$`,
		Dir:     directory,
		Env: []string{
			"ACP_TEST_HELPER=1",
			"ACP_TEST_BINARY=" + os.Args[0],
			"ACP_TEST_ENVIRONMENT_FILE=" + environmentFile,
			"CLAUDE_OAUTH_TOKEN=oauth-token",
			"CLAUDE_TEAM_HARNESS_PERSONA=project-manager",
			"CLAUDE_TEAM_HARNESS_MEMORY_DB=/private/memory.db",
			"CLAUDE_TEAM_HARNESS_BIN=/usr/local/bin/claude-team-harness",
			"PWD=/configured/pwd",
		},
		PermissionPolicy: PermissionDeny,
		Stderr:           io.Discard,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	contents, err := os.ReadFile(environmentFile)
	if err != nil {
		t.Fatalf("read adapter environment: %v", err)
	}
	got := map[string]string{}
	if err := json.Unmarshal(contents, &got); err != nil {
		t.Fatalf("decode adapter environment: %v", err)
	}
	for name, want := range map[string]string{
		"PATH":                          originalPath,
		"HOME":                          filepath.Join(directory, "home"),
		"TMPDIR":                        filepath.Join(directory, "tmp"),
		"PWD":                           directory,
		"CLAUDE_OAUTH_TOKEN":            "oauth-token",
		"CLAUDE_TEAM_HARNESS_PERSONA":   "project-manager",
		"CLAUDE_TEAM_HARNESS_MEMORY_DB": "/private/memory.db",
		"CLAUDE_TEAM_HARNESS_BIN":       "/usr/local/bin/claude-team-harness",
	} {
		if got[name] != want {
			t.Errorf("adapter %s = %q, want %q", name, got[name], want)
		}
	}
	for _, name := range []string{
		"WEBEX_BOT_TOKEN", "CLAUDE_TEAM_HARNESS_API_TOKEN", "UNRELATED_SECRET",
	} {
		if _, exists := got[name]; exists {
			t.Errorf("adapter unexpectedly received %s", name)
		}
	}
}

func TestACPAdapterProcess(t *testing.T) {
	if os.Getenv("ACP_TEST_HELPER") != "1" {
		return
	}
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found {
			environment[name] = value
		}
	}
	data, err := json.Marshal(environment)
	if err != nil {
		t.Fatalf("marshal environment: %v", err)
	}
	if err := os.WriteFile(os.Getenv("ACP_TEST_ENVIRONMENT_FILE"), data, 0o600); err != nil {
		t.Fatalf("write environment: %v", err)
	}
	decoder := json.NewDecoder(os.Stdin)
	var request message
	if err := decoder.Decode(&request); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	sendTestMessage(t, os.Stdout, message{
		JSONRPC: "2.0", ID: request.ID, Result: marshal(map[string]any{
			"protocolVersion": protocolVersion,
			"_meta": map[string]any{
				"steering": map[string]bool{"supported": false},
			},
		}),
	})
	for decoder.Decode(&request) == nil {
	}
}
