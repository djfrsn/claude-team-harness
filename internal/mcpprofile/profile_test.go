package mcpprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveInjectsOnlyNamedEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	data := `{"profiles":{"read":{"servers":[{"name":"jira","command":"jira-mcp","args":["--read-only"],"env_from":["JIRA_TOKEN"]}]}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write profiles: %v", err)
	}
	set, err := Load(path, func(name string) string {
		if name == "JIRA_TOKEN" {
			return "secret-value"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	servers, err := set.Resolve("read")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	env := servers[0]["env"].([]map[string]string)
	if len(env) != 1 || env[0]["name"] != "JIRA_TOKEN" || env[0]["value"] != "secret-value" {
		t.Fatalf("resolved environment = %+v", env)
	}
}
