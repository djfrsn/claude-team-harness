# Claude Team Harness

This repository runs one or more configured Claude Code personas behind a small,
transport-neutral HTTP API. It uses the
[Claude ACP adapter](https://github.com/agentclientprotocol/claude-agent-acp),
which translates the Agent Client Protocol (ACP) to the Claude Agent SDK.

ACP is the process protocol inside the service. It is JSON-RPC over the adapter
process standard input and output. The Go service supplies the HTTP boundary.

## Request flow

1. A Webex webhook handler receives and verifies a Webex event.
2. The handler stores the event in SQLite and returns `202`.
3. A worker fetches the Webex message and selects its conversation.
4. A main-room message uses `room:<roomId>`. A thread reply uses
   `thread:<roomId>:<parentId>`.
5. The router selects an explicit or mentioned persona, the conversation's
   saved persona, or the default persona.
6. That persona's pool resumes or opens its ACP session and sends the message
   to `claude-agent-acp`.
7. Claude Code reads `CLAUDE.md`, the persona prompt, and its MCP tools.
8. The worker posts the reply to the room or source thread.

An external daily scheduler can use `POST /v1/messages` with the room's
conversation ID to start a morning digest. The scheduler polls the run and can
send the completed reply through Webex. The harness includes inbound Webex
delivery, Claude sessions, and Jira and Confluence tool access. The external
scheduler owns scheduled outbound Webex delivery.

## Conversation lifecycle

Each persona keeps an independent ACP session for each room and thread. SQLite
stores persona routes, runs, ACP session IDs, context generations, completed
turn counts, handoffs, message history, runtime events, and accepted Webex
events.

The service calls ACP `session/load` the first time it uses a conversation after
a restart. It calls `session/new` when the saved session is missing or when the
conversation reaches `-max-session-turns` (default 50). Automatic rotation sends
the new session a bounded transcript handoff so the conversation continues.

The `fresh` mode creates a new context epoch. It sends only the current message
and limits every later handoff to the new epoch. A new thread starts with
its root message and the agent's answer to that root when available.

## Install

Requirements:

- Go 1.25.12 or later
- Claude Code with a valid company-approved login
- `@agentclientprotocol/claude-agent-acp`

Install the adapter:

```sh
npm install --global @agentclientprotocol/claude-agent-acp@0.66.0
```

Build the service:

```sh
go build -o claude-team-harness ./cmd/claude-team-harness
```

The Go binary and Node.js must use the same processor architecture. On Apple
Silicon, use `GOARCH=arm64 go build` when `go env GOARCH` reports `amd64` but
`node -p process.arch` reports `arm64`.

Edit `CLAUDE.md` for shared policy. Persona files in `config/personas` set each
role. The sample roster enables `project-manager` and `engineer`; it keeps
`researcher` as a disabled example. A roster with one enabled persona uses the
same runtime path.

## Command-line check

The command path starts Claude, resumes or opens the named conversation, sends
the prompt, prints the reply, and exits:

```sh
./claude-team-harness prompt -permission-policy allow_once \
  -conversation room:delivery \
  -persona project-manager \
  "Read Jira and Confluence, then prepare today's project report."
```

Use `-session-mode fresh` to discard that conversation's prior model context.

The default permission policy is `deny`. Use `allow_once` only when the Claude
process and its MCP servers run with approved access. A persona can set
`permission_policy` to override this default.

## HTTP API

Start the service with a bearer token:

```sh
CLAUDE_TEAM_HARNESS_API_TOKEN='<token>' ./claude-team-harness serve \
  -personas ./config/personas \
  -mcp-profiles ./config/mcp-profiles.json
```

The default address is `127.0.0.1:8080`. The process requires
`CLAUDE_TEAM_HARNESS_API_TOKEN` when the address is not a loopback address.

Set credentials in the process environment or in `<cwd>/.env`. Use `-env-file`
to select another file. Each component requests only its named values: the ACP
adapter receives `CLAUDE_OAUTH_TOKEN`, Webex uses its Webex values, and each MCP
server receives the values named by its profile. Keep `.env` outside source
control.

Check health:

```http
GET /healthz
```

List the enabled personas:

```http
GET /v1/personas
Authorization: Bearer <token>
```

The sample response contains `project-manager` as the default persona and
`engineer` as a second enabled persona.

Send a message:

```http
POST /v1/messages
Authorization: Bearer <token>
Content-Type: application/json

{
  "conversation_id": "delivery",
  "persona": "project-manager",
  "message_id": "scheduler:2026-08-13",
  "text": "Prepare today's project report."
}
```

The service stores the message and returns immediately:

```http
HTTP/1.1 202 Accepted
Location: /v1/runs/run:...
Retry-After: 2
```

```json
{
  "conversation_id": "delivery",
  "persona": "project-manager",
  "run_id": "run:...",
  "status": "queued",
  "status_url": "/v1/runs/run:..."
}
```

Poll the run until it is complete:

```http
GET /v1/runs/run:...
Authorization: Bearer <token>
```

```json
{
  "conversation_id": "delivery",
  "persona": "project-manager",
  "run_id": "run:...",
  "status": "completed",
  "status_url": "/v1/runs/run:...",
  "reply": "The release is at risk because ...",
  "stop_reason": "end_turn",
  "generation": 1,
  "cached": false,
  "steered": false
}
```

`conversation_id` is the core transport-neutral key. Webex adapters can use
`room_id` and `root_message_id`; these derive `room:<roomId>` or
`thread:<roomId>:<rootMessageId>`. `persona` selects an enabled persona. An
omitted persona follows an `@name` mention, the conversation's prior route, or
the default. `session_mode` accepts `continue` or `fresh`. Repeated `message_id`
values return the first run and preserve one Claude turn. Reusing a message ID
with different content returns `409`.

Run status is `queued`, `running`, `completed`, or `failed`. Queue workers use
`-turn-timeout` to bound each turn. Queued work survives a restart. A run that
was active during an interrupted process returns to the queue at startup.

Add `Prefer: wait=30` to wait for up to 30 seconds. The service returns `200`
with the completed run when it finishes during that interval. It returns `202`
with the current state when the wait expires. The configured turn timeout is
the maximum accepted wait.

A second message for the same persona and conversation steers the live turn
when the ACP adapter supports steering. The steering submission has its own run
resource. When delivered, that resource completes with `steered: true` and
`active_run_id` set to the turn it changed. A steering miss becomes the next
normal turn.

## MCP profiles and credentials

Copy `config/mcp-profiles.example.json` to the ignored
`config/mcp-profiles.json` file. A persona selects a profile with
`mcp_profile`. Profiles contain server commands, arguments, and `env_from`
names. They contain no credential values.

At startup, the harness checks that every selected profile exists. At session
start, it resolves each named environment value and passes that value only to
the selected MCP server through ACP `session/new`. Start with a read-only Jira
and Confluence account. Keep `WEBEX_BOT_TOKEN` in the Webex adapter and
`CLAUDE_OAUTH_TOKEN` in the ACP adapter.

## Webex webhook

Set the Webex bot token and the webhook secret before `serve` starts:

```sh
WEBEX_BOT_TOKEN='<bot-token>' \
WEBEX_WEBHOOK_SECRET='<webhook-secret>' \
CLAUDE_TEAM_HARNESS_API_TOKEN='<api-token>' \
./claude-team-harness serve -listen 0.0.0.0:8080 -permission-policy allow_once
```

Configure the Webex `messages.created` webhook target as:

```text
https://<service-host>/v1/webex/events
```

`X-Spark-Signature` authenticates the Webex endpoint independently from the
generic API bearer token. The worker ignores messages authored by its own bot,
deduplicates Webex retries, retries failed work up to five times, and replies
with `parentId` when the source message belongs to a thread. Webex replies start
with the selected persona name. Concurrent workers let a later room or thread
message steer a live turn.

The default state file is `.claude-team-harness/state.db`. Back it up with the
service data and keep it outside source control. Use `-state-db` to place it on
a persistent volume.

Use one stable room ID and message ID per scheduled report. This makes the daily
job continue the room conversation and makes a retried schedule idempotent.

## Quality gate

Run the same checks used by hooks and CI:

```sh
bash scripts/gate/check.sh
```

The gate checks shell and workflow files, Go format and lint rules, known Go
vulnerabilities, tests, race safety in CI, coverage, file length, and the source
line budget.

Enable the Git pre-commit hook once per clone:

```sh
git config core.hooksPath scripts/githooks
```

Claude Code reads `.claude/settings.json`. It checks edited Go and workflow
files at once and runs the full gate before Claude stops.
