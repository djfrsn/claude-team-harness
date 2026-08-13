package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/your-company/claude-team-harness/internal/memory"
)

const (
	memoryDBEnvironment = "CLAUDE_TEAM_HARNESS_MEMORY_DB"
	memoryPersonaEnv    = "CLAUDE_TEAM_HARNESS_PERSONA"
	harnessBinaryEnv    = "CLAUDE_TEAM_HARNESS_BIN"
	defaultMemoryDB     = ".claude-team-harness/memory.db"
)

const memoryUsage = `claude-team-harness memory manages one bounded Markdown document per persona.

The calling persona can replace its own document. A document over 500 lines is
rejected. A document over 450 lines causes each agent turn to request pruning.

Usage:
  claude-team-harness memory read [--as <persona>] [--out <directory>]
  claude-team-harness memory write [--as <persona>] --file <path>
`

func runMemory(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("memory needs a subcommand")
	}
	switch args[0] {
	case "help", "-help", "--help":
		_, err := io.WriteString(stdout, memoryUsage)
		return err
	case "read":
		return memoryRead(ctx, args[1:], stdout)
	case "write":
		return memoryWrite(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("unknown memory subcommand %q\n%s", args[0], memoryUsage)
	}
}

func memoryRead(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("memory read", flag.ContinueOnError)
	flags.SetOutput(stdout)
	as := flags.String("as", "", "persona name for an operator read")
	out := flags.String("out", "", "directory that receives memory.md")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory read does not accept positional arguments")
	}
	persona, err := memoryPersona(*as)
	if err != nil {
		return err
	}
	return withMemory(ctx, func(store *memory.Store) error {
		doc, err := store.Read(ctx, persona)
		if err != nil {
			return err
		}
		if *out != "" {
			if err := os.MkdirAll(*out, 0o700); err != nil {
				return fmt.Errorf("create memory output directory: %w", err)
			}
			path := filepath.Join(*out, "memory.md")
			if err := os.WriteFile(path, []byte(doc.Body), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"memory": doc})
	})
}

func memoryWrite(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("memory write", flag.ContinueOnError)
	flags.SetOutput(stdout)
	as := flags.String("as", "", "persona name for an operator write")
	file := flags.String("file", "", "Markdown document to store")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory write does not accept positional arguments")
	}
	if *file == "" {
		return errors.New("memory write needs --file <path>")
	}
	persona, err := memoryPersona(*as)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read %s: %w", *file, err)
	}
	return withMemory(ctx, func(store *memory.Store) error {
		doc, err := store.Write(ctx, time.Now().UTC(), persona, string(body))
		if err != nil {
			return err
		}
		doc.Body = ""
		return json.NewEncoder(stdout).Encode(map[string]any{"memory": doc})
	})
}

func memoryPersona(as string) (string, error) {
	if persona := os.Getenv(memoryPersonaEnv); persona != "" {
		if as != "" {
			return "", fmt.Errorf(
				"memory: this session's persona is %s (%s); drop --as",
				persona, memoryPersonaEnv,
			)
		}
		return persona, nil
	}
	if as == "" {
		return "", fmt.Errorf("memory needs --as <persona> when %s is not set", memoryPersonaEnv)
	}
	return as, nil
}

func memoryDBPath() string {
	if path := os.Getenv(memoryDBEnvironment); path != "" {
		return path
	}
	return defaultMemoryDB
}

func withMemory(ctx context.Context, use func(*memory.Store) error) error {
	store, err := memory.Open(ctx, memoryDBPath())
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return use(store)
}
