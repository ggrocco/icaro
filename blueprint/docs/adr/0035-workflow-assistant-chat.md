# ADR 0035 — A workflow assistant chat that drives the user's own agent CLI

**Status:** Accepted. The open questions are settled below. Spike S11 is still open: it needs real subscription logins, so Phase 14 is built against stream fixtures modeled on the CLIs' documented formats, and S11 must pass on real logins before the assistant is released. Adds a third session kind, `assist`, and a chat surface to the web UI and the desktop app. Lifts part of the [deferred design](../02-decisions-risks.md#deferred-designs) "in-container MCP access" (the `author` level, for assistant sessions only, with human-applied writes). Builds on [ADR 0005](0005-persistent-home-egress.md) (profile homes), [ADR 0019](0019-memory-mcp-transport.md) (per-session MCP socket), [ADR 0028](0028-credential-injection.md) (`inject` profiles) and [architecture §10](../01-architecture.md#10-agent-interface) (authoring tools).

## Context

Authoring a workflow today means one of two things:

- editing the form or YAML in the web UI by hand;
- running an agent CLI on the host with the `icaro-workflows` skill and `icaro mcp` ([architecture §10](../01-architecture.md#10-agent-interface)).

The second converges well, because of the schema and `validate_workflow`, but it happens in a terminal, away from the editor, the run graph and the integrations page. Users want a chat panel next to the editor: describe the workflow, let an assistant call the authoring tools, and see the draft, its issues and a test run in the same window.

Three facts shape the design.

1. **Most users have a subscription, not an API key.** They sign in to Claude Code with Claude Pro/Max, or to Codex with ChatGPT, once per profile (`icaro login`). The CLI holds and refreshes its own OAuth token in the profile home (ADR 0005). The daemon has no provider key to call a model API with, and ADR 0028 explicitly does not cover subscription logins.
2. **The daemon must not take the subscription token.** Reading the OAuth token out of the home and calling the provider's API from the daemon would turn a credential that ADR 0005 keeps confined to one container into one the daemon handles. It is also not how the providers allow subscription access to be used: the supported client for a subscription is the provider's own CLI. The only client Ícaro may run against a subscription is that CLI, unchanged, under the user's login.
3. **The agent CLIs already run headless.** Workflow agent steps run them non-interactively, prompt in and transcript out ([architecture §6](../01-architecture.md#6-workflow-engine)). Claude Code (`claude -p --output-format stream-json`) and Codex (`codex exec --json`) both emit a machine-readable event stream, both can resume a conversation by id, and both load MCP servers from their home config.

So the model side of a chat is not an API client in the daemon. It is **one headless CLI turn per user message**, in a container, under the profile's login, with the authoring tools reachable over MCP. The same design works unchanged for an `inject` profile with an API key, because the CLI is the client either way.

## Decision

### Assistant sessions

A new session kind, `assist`, backs each chat (`sessions.kind = assist`). It follows the session spine (ADR 0002) and its reconciliation path (ADR 0006).

- **Profile.** The operator picks it in config, as for workflows (ADR 0010): `assistant.profile`, defaulting to the first profile whose `agent` is `claude`, then `codex`. A chat cannot pick a profile; the profile's egress allowlist bounds the container. The recommended allowlist is the provider's hosts only.
- **Workspace.** A scratch directory under `~/.icaro/assist/<chat>/workspace`, not a repository worktree. The assistant does not edit code; it edits workflows through tools. An optional `project` on the chat scopes memory reads, as for agent sessions.
- **Container.** One long-lived container per open chat, under the container baseline, idling until a turn arrives. It is stopped after `assistant.idle_timeout` (default 15 minutes) and started again on the next message; the conversation survives this, because it lives in the CLI's transcript in the session's home layer.
- **Concurrency.** Assistant sessions do not take `sessions.max_active` slots (they are idle most of the time) but have their own cap, `assistant.max_active` (default 2). A chat over the cap waits as `queued`, like an agent session.

### One turn is one headless CLI invocation

Each user message runs the profile's agent CLI once, with `docker exec`, in the chat's container. The agent catalog in `internal/creds` gains a **headless chat adapter** per agent, next to `Run` and `Login`:

| | Claude Code | Codex |
|---|---|---|
| First turn | `claude -p --output-format stream-json --verbose --session-id <uuid>` | `codex exec --json --skip-git-repo-check` |
| Next turns | `claude -p --output-format stream-json --verbose --resume <uuid>` | `codex exec resume <thread-id> --json` |
| Tools | `--strict-mcp-config --mcp-config <icaro only>`, `--allowedTools mcp__icaro__*`, built-in file, shell and web tools disallowed | `--sandbox read-only`, approval policy `never`, `icaro` as the only MCP server |
| System prompt | `--append-system-prompt` with the `icaro-workflows` skill and the chat preamble | the skill and preamble in the session layer's `AGENTS.md` |

The exact flags are pinned per CLI version by spike S11; the adapter, not the chat code, knows them. The prompt goes in on stdin, never on the command line, so it does not show in process listings.

The adapter also **parses the CLI's event stream** into one normalized stream: `text` (deltas), `tool_call` (name, arguments), `tool_result` (truncated), `usage` (tokens; cost only when the CLI reports API pricing), `rate_limited` (with the reset time when the CLI reports it), `auth_required`, `error` and `done`. The stream comes out of a container that runs model output, so it is **untrusted input**: the parser has a fuzz target, caps line length and event count, and drops what it does not know.

Why one process per turn, and not one long-lived process fed over stdin:

- A turn is a bounded unit: it has a timeout (`assistant.turn_timeout`, default 10 minutes), it can be cancelled with the exec, and a stuck CLI never holds a chat.
- A daemon restart loses only the turn in flight, which is marked `interrupted` with a **Retry** button. Exec streams cannot be re-attached, and a long-lived process would lose the same turn anyway.
- Conversation continuity is the CLI's own job (`--resume`), so the daemon stores the CLI's conversation id and the visible messages, not the model's context.

The cost is the CLI's start-up time on every turn. S11 measures it; a persistent process is the fallback if it is too slow.

### What the assistant can reach: the authoring tools, with human-applied writes

The chat's container gets its per-session MCP socket (ADR 0019). For an `assist` session, the socket serves the authoring tools of architecture §10 instead of only the memory tools. The caller's tool set comes from the session's kind, server-side, as scope does today; nothing the agent sends widens it.

| Tool | In an assistant session |
|---|---|
| `get_workflow_schema`, `list_integrations`, `get_integration`, `list_secrets`, `validate_workflow`, `get_workflow`, `list_workflows`, `get_run` | As on the host. `list_secrets` returns names and host bindings only, as everywhere. |
| `get_draft` (new) | Returns the chat's draft: the YAML currently in the editor, its issues and the workflow it edits. |
| `propose_workflow` (new) | Validates the YAML and stores it as the chat's **draft**, with its issues. It never writes a workflow. |
| `create_workflow`, `update_workflow` | **Not served.** The assistant proposes; the person applies. |
| `run_workflow` | Served, but **gated**: the call blocks until the person approves or declines a run card in the chat (or `assistant.approval_timeout`, default 5 minutes, passes), and returns the run id or `declined`. |
| memory tools | Read only, when the chat has a project. |

**Why writes go through a draft.** The chat is driven by a person, but the assistant also reads text it did not write: run logs through `get_run`, manifests from third-party packs, memory. Any of these can carry a prompt injection. The deferred design already says agent-authored workflows land as inert drafts until a human approves them; a draft that the person applies from the editor is that rule, made cheap enough to use on every edit. Applying is an ordinary `ApplyWorkflow` from the UI, recorded with actor `<user> via assistant <chat>`, so version history shows where an edit came from.

**Why runs are gated in Ícaro, not in the CLI.** Claude Code's permission prompts do not exist in headless mode, and Codex's work differently. A gate inside the daemon's tool works for every agent, shows the exact workflow and input that would run, and is decided by a person over the authenticated API (`write` scope, CSRF-checked from the browser), like an `approval` step ([ADR 0030](0030-approval-wait-steps.md)).

No tool deletes anything, and no tool returns a secret value, as in architecture §10.

### The draft is shared with the editor

The chat is opened from the editor, or standalone from a **New with assistant** button. Its draft and the editor's document are one value:

- When the person edits the form or YAML, the UI sends the new YAML to the chat (debounced); `get_draft` returns it.
- When the assistant calls `propose_workflow`, the editor shows the proposal as a diff against its document, with the validator's issues, and **Accept** / **Discard**. Accept replaces the editor's document; saving still goes through **Validate & save**.
- A chat about an existing workflow starts with that workflow's latest version as its draft.

### Persistence and streaming

- New tables, in both dialects: `chats` (id, session id, profile, agent, project, workflow name, CLI conversation id, status, created and updated times), `chat_messages` (id, chat, role, short text, status, token counts, time) and `chat_drafts` (chat, YAML, issues JSON, source `user` or `assistant`, time).
- Message text is capped in SQL. Tool arguments and results, and the raw CLI stream, go to an append-only, secret-redacted log per chat (`~/.icaro/logs/assist/<chat>.jsonl`), as for step logs; a message row points into it.
- The daemon publishes the normalized stream on the event bus; the UI follows it over SSE (`GET /api/v1/chats/{id}/events`), the same way it follows a run.
- A goroutine per chat owns that chat's turn state and serializes its turns: a message sent while a turn runs is queued behind it, never run in parallel against the same conversation.

### Subscription specifics

- **Login.** A chat whose profile is not signed in shows **Sign in required**, with the `icaro login <profile>` command. The browser cannot complete a CLI OAuth flow, so login stays in the terminal (or a later desktop-app terminal). `auth_required` from the adapter, and a doctor check ("assistant profile signed in"), feed the same state.
- **Rate limits.** Subscriptions have usage windows, not budgets. `rate_limited` ends the turn with the reset time when the CLI gives one; the daemon never retries a turn on its own, so a chat cannot burn a window in the background.
- **Usage.** Tokens per turn come from the CLI's own usage fields and are stored on the message. A subscription turn shows tokens, not money; the CLI's dollar figure is shown only for `inject` (API-key) profiles, and labelled an estimate as in [ADR 0022](0022-usage-from-local-logs.md). The chat's transcripts sit in a profile home, so `icaro usage` counts them like any other session's; where the session layer is a Docker volume, the turn's `usage` events are the source instead.
- **API keys.** An `inject` profile needs nothing extra: the CLI gets the placeholder key and base URL of ADR 0028, and the injector's per-session audit log attributes the chat's calls.

### Clients

- **API:** `POST /api/v1/chats`, `GET /api/v1/chats`, `GET /api/v1/chats/{id}`, `POST /api/v1/chats/{id}/messages`, `POST /api/v1/chats/{id}/cancel`, `PUT /api/v1/chats/{id}/draft`, `POST /api/v1/chats/{id}/approvals/{call}` (approve or decline a gated call), `DELETE /api/v1/chats/{id}`, and the SSE stream.
- **Web UI:** an assistant panel docked beside the editor, and an **Assistant** page listing chats. The desktop app shows the same pages; the tray gains **Ask the assistant**.
- **CLI:** `icaro assist` (`list`, `show`, `send`, `rm --yes`), a thin client, so every chat action is reachable and testable without a browser. It is not meant to replace running the agent CLI on the host with the skill, which keeps working.

## Alternatives considered

- **Call the Messages API from the daemon.** The simplest chat, but it needs an API key, which most users do not have. Using the subscription's OAuth token for it would take a credential out of the CLI's control, and is not a supported use of a subscription. It would also make the daemon depend on a specific provider, which [05-engineering](../05-engineering.md#forbidden-without-an-adr) forbids without an ADR. Rejected; an API-key user gets the same chat through an `inject` profile.
- **Run the agent CLI on the host with the user's own login.** No container start, and it reuses an existing host login. But the daemon would start agent processes on the host (MVP cut 4) with the user's whole home in reach, which ADR 0005 and ADR 0011 rule out. Running the CLI on the host with the skill and `icaro mcp` stays the terminal path; it is the user's choice there, not the daemon's.
- **Embed an interactive terminal (PTY) in the UI.** Works for every CLI with no adapter, but gives no structured events: no tool calls to render, no draft to diff, no approval cards. It may still come later for `icaro login` in the desktop app.
- **Let the assistant write workflows directly**, as the host MCP server does. Simpler, and the version history allows a revert. Rejected for the injection reason above; the draft costs one click.

## Settled questions

1. **`run_workflow` is served**, behind the approval card described above.
2. **One process per turn** until S11 measures the start-up time on real logins; a persistent process stays the fallback behind the same adapter.
3. **A chat keeps its container between turns**, stopped after `assistant.idle_timeout`.
4. **Chats are listed globally**, each with an optional project that scopes memory reads.

## Consequences

- Users with a subscription get an in-app assistant without an API key, and Ícaro never handles their OAuth token. The same code serves API-key users through ADR 0028.
- The assistant is only as capable as the CLI in the profile, and each CLI version can change its flags or event format. The adapter is the one place that knows them; recorded streams from each supported version are test fixtures, and an unknown event never breaks a turn.
- Whether a provider's terms allow a subscription CLI to be driven headless by a local tool is the user's agreement with that provider. Ícaro runs the official CLI unchanged, under the user's own login, on the user's machine, as its workflow agent steps already do; S11 records what each provider documents.
- The assistant cannot write a workflow, delete anything or read a secret value, and every run it starts was approved by a person. A prompt-injected assistant can still propose a bad draft; the diff and the validator are what the person reviews.
- A chat adds one idle container while it is open, bounded by `assistant.max_active` and `assistant.idle_timeout`.
- Every turn counts against the subscription's usage window, and the chat shows it.
