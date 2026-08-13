# Persona Memory

## Change

Give each configured persona one bounded Markdown memory document. Match Agent
Studio's memory behavior: the persona reads its document when an ACP session
opens, can replace it through the harness CLI, and receives a prune instruction
when the document grows beyond the working limit.

## Requirements

- As a persona, when a new ACP session opens, I receive my own memory document
  after my persona prompt.
- As a persona, I can read and replace my own memory document through the
  harness CLI without naming another persona.
- As an operator, I can read or replace a selected persona's memory from a
  terminal.
- As an operator, each persona's memory stays separate from every other
  persona's memory.
- As an operator, a write over 500 lines fails without changing the stored
  document.
- As a persona, when my memory exceeds 450 lines, each turn tells me to prune
  it to 450 lines or fewer.
- As an operator, a memory read failure does not stop an agent turn.

## Design

The `memory` package stores one Markdown document per normalized persona name in
a separate SQLite database. The service passes an absolute database path, the
persona name, and the current harness executable path into each persona's ACP
adapter environment.

The CLI exposes `memory read` and `memory write --file`. A session-bound persona
comes from `CLAUDE_TEAM_HARNESS_PERSONA`; while it is set, the command rejects
`--as`. A terminal uses `--as <persona>`.

The conversation manager reads memory for each turn. A new ACP session receives
the persona prompt, full memory block, saved conversation handoff, and current
message. A warm session receives the current message and any required prune
instruction. Memory read failures are logged and the turn continues.

## Test

Store tests cover empty reads, replacement, normalization, persona separation,
line counting, and the write cap. CLI tests cover self identity, terminal
identity, cross-persona rejection, file input, and file output. Conversation
tests cover fresh and warm framing plus pruning. The release proof asks a real
Claude persona to write a codeword, starts another conversation, and checks that
the new session recalls the codeword from its injected memory.

## Non-goals

- Shared team, room, or thread memory
- Search, embeddings, or automatic extraction
- Partial document updates or revision history
- HTTP memory administration
