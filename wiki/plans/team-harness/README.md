# Claude Team Harness

## Change

Create a small harness that runs configured Claude Code personas through ACP.
Provide the same message operation through a command and HTTP.

Extend the service with durable conversation scopes. Each Webex room owns one
conversation. Each Webex thread owns another conversation. The service resumes
the current ACP session after a restart and rotates to a new session with a
bounded transcript handoff after the configured turn limit.

Extend the harness with a persona roster, one ACP process pool per persona, and
live steering. MCP profiles select tool servers and name the environment
variables that supply their credentials.

Make generic HTTP message submission durable and asynchronous. A caller can
poll a run resource or request a short bounded wait.

## Requirements

- As a Webex integration, I can send text and receive one Claude reply.
- As an operator, I can test the agent from the command line before I run HTTP.
- As a security owner, I can require a bearer token and select the ACP tool
  permission policy.
- As an engineer, I can add Jira and Confluence MCP servers without a code
  change.
- As a room member, when I post a main-room message, I receive an answer from
  the room conversation.
- As a thread member, when I reply in a thread, I receive an answer from that
  thread's independent conversation.
- As a room member, when a conversation reaches its turn limit, my next message
  continues in a new Claude session with prior context.
- As an operator, when the service restarts, each conversation resumes its
  current Claude session when Claude still has it.
- As an API caller, when I select `fresh`, my message starts a new session with
  the base persona and current message.
- As a Webex administrator, when Webex retries a message webhook, the agent
  processes the message once.
- As a room member, I can address a configured persona by name and keep that
  persona's room or thread context separate from other personas.
- As an operator, I can run several persona turns at the same time within a
  global process limit.
- As a room member, a message that arrives during a turn steers that turn when
  the ACP adapter supports steering and becomes a later turn when it does not.
- As a security owner, each persona receives only its selected MCP profile and
  each MCP server receives only the environment values named by that profile.
- As an operator, I can inspect durable runs and steering events without storing
  credentials in runtime events.
- As an API caller, when I submit a message, I receive a run ID before Claude
  completes the turn.
- As an API caller, I can poll the run ID and read its queued, running,
  completed, or failed state.
- As an API caller, I can request a bounded wait and receive the result when it
  completes within that wait.
- As an API caller, a repeated message ID resolves to the first queued run.
- As an operator, queued work survives a service restart and processing work
  returns to the queue after an interrupted process.
- As an operator, when an ACP adapter does not answer cancellation, the turn
  ends within a fixed grace period and releases its worker.
- As a room member, when an ACP adapter becomes unavailable, my next turn uses
  a replacement in the same pool slot and resumes the stored session.
- As a security owner, the ACP adapter receives only the environment values
  required to start Claude and run its selected MCP servers.
- As an API caller, when a queued turn fails or times out, its run reaches the
  failed state with a useful bounded error.
- As an operator, when shutdown interrupts a queued turn, the same run ID
  returns to the queue and completes after restart.
- As a room member, when a second queued message reaches an active turn, the
  second run records steering against the active run.
- As a room member, when a saved ACP session is missing, a replacement session
  receives the saved handoff. Other session-load failures stop the turn.

## Design

The process starts one `claude-agent-acp` process for each configured pool slot
and uses newline-delimited JSON-RPC on standard input and output. Claude Code
reads `CLAUDE.md` for shared policy and the selected persona prompt for its
role. SQLite stores conversations, messages, and accepted Webex hooks. A
conversation key is `room:<roomId>` for a top-level message and
`thread:<roomId>:<parentId>` for a thread reply.

Each conversation stores its ACP session ID, generation, completed turn count,
and bounded handoff. The first use after process start calls `session/load`.
A missing Claude session opens a replacement with the saved handoff. A turn
limit opens the next generation with the handoff. A `fresh` request opens the
next generation with only the current message.

Persona files under `config/personas` define the roster. Routing selects an
explicit API persona, then the first valid `@name`, then the conversation's
saved owner, then the default persona. Conversation keys include the persona,
for example `project-manager|thread:<roomId>:<parentId>`.

Each persona owns a pool of ACP adapter processes. A pool runs one turn per
slot, routes live messages to ACP steering, and handles a steering miss as the
next normal turn. A global limit bounds all persona processes. The sample roster
enables a project manager and an engineer to prove routing and context isolation.
A one-persona roster uses the same API and runtime path.

MCP profiles use a file-backed JSON format. The checked-in example contains
commands, arguments, and environment-variable names. Runtime values come from
the process environment or `.env` and go only to the selected MCP subprocess.
Webex credentials stay in the Webex client and Claude credentials stay in the
ACP adapter.

The ACP client sends cancellation when a turn context ends. It waits for a
short grace period, then returns the context error and releases the worker. The
adapter process receives a small system environment plus values that the
harness supplies for Claude and the selected MCP profile. The next normal turn
replaces an unavailable adapter and loads the durable session into the new
slot process.

The HTTP service exposes `GET /healthz`, `GET /v1/personas`,
`POST /v1/messages`, `GET /v1/runs/{runId}`, and `POST /v1/webex/events`.
Message submission writes a durable queue row and returns `202`. Run workers
call the same transport-neutral team runtime used by the command and Webex
adapter. `Prefer: wait=<seconds>` waits up to a bounded interval. The Webex
endpoint verifies the webhook secret, stores the message event, and returns
`202`. Workers fetch messages, ignore bot messages, route them, run Claude, and
post labeled replies. Concurrent workers let a later room message steer a live
turn. Selected MCP profile definitions go into the ACP session request.

## Test

Unit tests drive the ACP request and streamed reply across in-memory pipes. HTTP
tests check authentication, persona discovery, asynchronous submission, bounded
waits, run polling, input validation, Webex signatures, and scope routing.
Store and manager tests check queue recovery, idempotency, restart resume,
independent persona, room, and thread sessions, stable pool slots, live
steering, rotation handoff, fresh sessions, and webhook deduplication.
Failure-path tests check bounded ACP cancellation, adapter credential scope,
failed run state, shutdown requeue, worker restart, queue-to-ACP steering,
missing-session replacement, unavailable-slot replacement, and session-load
failure handling.
`go test ./...` and live HTTP prompts provide the release checks.
