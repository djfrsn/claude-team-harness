package conversation

import (
	"context"
	"fmt"

	"github.com/your-company/claude-team-harness/internal/memory"
)

func (m *Manager) memoryFraming(ctx context.Context) (block, prune string) {
	if m.cfg.Memory == nil {
		return "", ""
	}
	doc, err := m.cfg.Memory.Read(ctx, m.cfg.Persona)
	if err != nil {
		m.cfg.Logf("memory read %s: %v", m.cfg.Persona, err)
		return "", ""
	}
	return memoryBlock(doc), pruneNote(doc.Lines)
}

func memoryBlock(doc memory.Document) string {
	head := fmt.Sprintf("[Your memory — %d lines]\n", doc.Lines) +
		"This is your own memory document, loaded when a new session opens. " +
		"When this turn changes what your future self should know, write the " +
		"full updated document to a file and run " +
		"`\"$CLAUDE_TEAM_HARNESS_BIN\" memory write --file <path>`."
	if doc.Body == "" {
		return head + "\nIt is empty: record your decisions, relationships, " +
			"and unfinished work."
	}
	return head + "\n\n" + doc.Body
}

func pruneNote(lines int) string {
	if lines <= memory.PruneLines {
		return ""
	}
	return fmt.Sprintf("Your memory is %d lines, above the %d-line prune "+
		"limit. Before this turn ends, rewrite it to keep the information "+
		"most useful to your future decisions, relationships, and unfinished "+
		"work. Merge repeated points, remove details available elsewhere, "+
		"and finish at %d lines or fewer.",
		lines, memory.PruneLines, memory.PruneLines)
}

func prependMemory(block, prompt string) string {
	if block == "" {
		return prompt
	}
	return block + "\n\n" + prompt
}

func appendPrune(prompt, prune string) string {
	if prune == "" {
		return prompt
	}
	return prompt + "\n\n" + prune
}
