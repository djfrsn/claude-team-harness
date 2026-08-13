// Package persona loads and routes a file-backed agent roster.
package persona

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Persona struct {
	Name             string
	DisplayName      string
	Description      string
	Runtime          string
	Slots            int
	Default          bool
	Disabled         bool
	MCPProfile       string
	PermissionPolicy string
	Prompt           string
}

type frontmatter struct {
	Name             string `yaml:"name"`
	DisplayName      string `yaml:"display_name"`
	Description      string `yaml:"description"`
	Runtime          string `yaml:"runtime"`
	Slots            int    `yaml:"slots"`
	Default          bool   `yaml:"default"`
	Disabled         bool   `yaml:"disabled"`
	MCPProfile       string `yaml:"mcp_profile"`
	PermissionPolicy string `yaml:"permission_policy"`
}

type Set struct {
	personas []Persona
}

type UnknownError struct {
	Name string
}

func (e UnknownError) Error() string {
	return fmt.Sprintf("unknown persona %q", e.Name)
}

func Load(directory string) (*Set, error) {
	paths, err := filepath.Glob(filepath.Join(directory, "*.persona.md"))
	if err != nil {
		return nil, fmt.Errorf("persona: list %s: %w", directory, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("persona: no persona files in %s", directory)
	}
	sort.Strings(paths)
	personas := make([]Persona, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("persona: read %s: %w", path, err)
		}
		parsed, err := parse(data)
		if err != nil {
			return nil, fmt.Errorf("persona: %s: %w", path, err)
		}
		personas = append(personas, parsed)
	}
	return NewSet(personas)
}

func parse(data []byte) (Persona, error) {
	front, body, err := splitFrontmatter(string(data))
	if err != nil {
		return Persona{}, err
	}
	var values frontmatter
	decoder := yaml.NewDecoder(strings.NewReader(front))
	decoder.KnownFields(true)
	if err := decoder.Decode(&values); err != nil && !errors.Is(err, io.EOF) {
		return Persona{}, fmt.Errorf("frontmatter: %w", err)
	}
	result := Persona{
		Name:        strings.ToLower(strings.TrimSpace(values.Name)),
		DisplayName: strings.TrimSpace(values.DisplayName),
		Description: strings.TrimSpace(values.Description),
		Runtime:     strings.TrimSpace(values.Runtime),
		Slots:       values.Slots, Default: values.Default, Disabled: values.Disabled,
		MCPProfile:       strings.TrimSpace(values.MCPProfile),
		PermissionPolicy: strings.TrimSpace(values.PermissionPolicy),
		Prompt:           strings.TrimSpace(body),
	}
	if result.Slots <= 0 {
		result.Slots = 1
	}
	switch {
	case result.Name == "":
		return Persona{}, errors.New("name is required")
	case result.DisplayName == "":
		return Persona{}, fmt.Errorf("persona %q needs display_name", result.Name)
	case result.Description == "":
		return Persona{}, fmt.Errorf("persona %q needs description", result.Name)
	case result.Prompt == "" && !result.Disabled:
		return Persona{}, fmt.Errorf("persona %q needs a prompt", result.Name)
	}
	return result, nil
}

func splitFrontmatter(text string) (string, string, error) {
	rest, found := strings.CutPrefix(text, "---\n")
	if !found {
		return "", "", errors.New("file must start with a `---` line")
	}
	front, body, found := strings.Cut(rest, "\n---\n")
	if !found {
		return "", "", errors.New("frontmatter needs a closing `---` line")
	}
	return front, body, nil
}

func NewSet(personas []Persona) (*Set, error) {
	seen := make(map[string]bool, len(personas))
	defaults := 0
	stored := make([]Persona, len(personas))
	for index, value := range personas {
		value.Name = strings.ToLower(strings.TrimSpace(value.Name))
		if seen[value.Name] {
			return nil, fmt.Errorf("persona: duplicate name %q", value.Name)
		}
		seen[value.Name] = true
		if value.Default && !value.Disabled {
			defaults++
		}
		stored[index] = value
	}
	if defaults != 1 {
		return nil, fmt.Errorf("persona: need one enabled default, found %d", defaults)
	}
	return &Set{personas: stored}, nil
}

func (s *Set) Enabled() []Persona {
	result := make([]Persona, 0, len(s.personas))
	for _, value := range s.personas {
		if !value.Disabled {
			result = append(result, value)
		}
	}
	return result
}

func (s *Set) Lookup(name string) (Persona, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, value := range s.personas {
		if !value.Disabled && value.Name == name {
			return value, true
		}
	}
	return Persona{}, false
}

func (s *Set) Default() Persona {
	for _, value := range s.personas {
		if value.Default && !value.Disabled {
			return value
		}
	}
	return Persona{}
}

func (s *Set) Route(text, savedOwner string) Persona {
	for _, name := range mentions(text) {
		if value, found := s.Lookup(name); found {
			return value
		}
	}
	if value, found := s.Lookup(savedOwner); found {
		return value
	}
	return s.Default()
}

func mentions(text string) []string {
	var result []string
	for index := 0; index < len(text); index++ {
		if text[index] != '@' || index > 0 && isNameCharacter(text[index-1]) {
			continue
		}
		end := index + 1
		for end < len(text) && isNameCharacter(text[end]) {
			end++
		}
		if end > index+1 {
			result = append(result, strings.ToLower(text[index+1:end]))
		}
		index = end - 1
	}
	return result
}

func isNameCharacter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-'
}
