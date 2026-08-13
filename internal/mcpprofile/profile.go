// Package mcpprofile resolves checked-in MCP definitions with runtime secrets.
package mcpprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/your-company/claude-team-harness/internal/acp"
)

type File struct {
	Profiles map[string]Profile `json:"profiles"`
}

type Profile struct {
	Servers []Server `json:"servers"`
}

type Server struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	EnvFrom []string `json:"env_from"`
}

type Set struct {
	profiles map[string]Profile
	lookup   func(string) string
}

func Load(path string, lookup func(string) string) (*Set, error) {
	if lookup == nil {
		lookup = os.Getenv
	}
	if path == "" {
		return &Set{profiles: map[string]Profile{}, lookup: lookup}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read MCP profiles: %w", err)
	}
	var file File
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode MCP profiles: %w", err)
	}
	return &Set{profiles: file.Profiles, lookup: lookup}, nil
}

func (s *Set) Resolve(name string) ([]acp.MCPServer, error) {
	if name == "" {
		return nil, nil
	}
	profile, found := s.profiles[name]
	if !found {
		return nil, fmt.Errorf("MCP profile %q does not exist", name)
	}
	servers := make([]acp.MCPServer, 0, len(profile.Servers))
	for _, server := range profile.Servers {
		if server.Name == "" || server.Command == "" {
			return nil, fmt.Errorf("MCP profile %q has a server without name or command", name)
		}
		envNames := append([]string(nil), server.EnvFrom...)
		sort.Strings(envNames)
		env := make([]map[string]string, 0, len(envNames))
		for _, variable := range envNames {
			value := s.lookup(variable)
			if value == "" {
				return nil, fmt.Errorf("MCP profile %q needs environment variable %s", name, variable)
			}
			env = append(env, map[string]string{"name": variable, "value": value})
		}
		servers = append(servers, acp.MCPServer{
			"name": server.Name, "command": server.Command,
			"args": append([]string(nil), server.Args...), "env": env,
		})
	}
	return servers, nil
}

func (s *Set) Validate(names []string) error {
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, found := s.profiles[name]; !found {
			return fmt.Errorf("MCP profile %q does not exist", name)
		}
	}
	return nil
}
