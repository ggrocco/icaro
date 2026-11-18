# Phase 14 — Workflow assistant chat

**Objective:** a chat beside the workflow editor that drives the profile's own agent CLI headless, under the user's subscription login or an `inject` API key, and authors workflows through the MCP tools, as proposed in [ADR 0035](../adr/0035-workflow-assistant-chat.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs ADR 0035 **accepted** (done, with its questions settled). Spike **S11** needs real subscription logins: the phase is built against fixtures modeled on the CLIs' documented stream formats, and S11 must pass, with its recorded fixtures replacing the modeled ones, before the assistant is released.

## Spike S11 — headless chat on a subscription login

Run before any other deliverable, on real Claude Pro/Max and ChatGPT sign-ins, inside an agent container of a profile whose allowlist holds only the provider's hosts.

- [ ] For each CLI (Claude Code, Codex), record the exact flags for a first turn and a resumed turn, with the prompt on stdin, the event stream on stdout, `icaro` as the only MCP server and the built-in shell, file and web tools off.
- [ ] Record streams as fixtures under `internal/assist/stream/testdata/<agent>/<version>/`, beside `modeled/`: plain text, a tool call and its result, a multi-tool turn, a resumed turn, a cancelled turn, an expired login, a rate limit and a CLI error.
- [ ] Measure the time from `exec` to the first `text` event, excluding model latency, over 20 turns. Pass: p95 under 3 s. Fail: a persistent process fed over stdin, behind the same adapter.
- [ ] Confirm the conversation resumes after the container is stopped and started again (the transcript lives in the session layer).
- [ ] Note what each provider documents about driving its CLI headless with a subscription, and record it in ADR 0035.

## Deliverables

- [x] **Agent adapters.** In `internal/creds`, each `Agent` gains a headless chat spec: the first-turn and resume commands, the MCP and system-prompt configuration for an `assist` session, and a stream parser. Parsing lives in a new `internal/assist/stream` package with a `FuzzParse` target, since the stream is untrusted output from a container; crashers go under `testdata/fuzz/`.
- [x] **Session kind.** `session.KindAssist`, with a scratch workspace, a long-lived idle container under the baseline and `assistant.profile`, idle stop after `assistant.idle_timeout`, and the `assistant.max_active` cap with `queued` sessions. Startup reconciliation marks a turn that was in flight `interrupted`.
- [x] **Chat service.** A new `internal/assist` package, wired in `internal/app`:
  - one goroutine per active chat owning its turn state, with turns serialized over a channel (no mutexes);
  - a turn runs the adapter with `docker exec`, honors `assistant.turn_timeout` and cancel, and never retries on its own;
  - normalized events go to the event bus and the per-chat redacted log.
- [x] **MCP tool set by session kind.** The per-session socket of an `assist` session serves the authoring tools of [architecture §10](../01-architecture.md#10-agent-interface) without `create_workflow` and `update_workflow`, plus `get_draft` and `propose_workflow`, and read-only memory when the chat has a project. The set is chosen server-side from the session's kind. `run_workflow` blocks on an approval card and returns the run id or `declined`.
- [x] **Store.** Migration `NNNNN_chats` in both dialects: `chats`, `chat_messages`, `chat_drafts`, plus the pending tool approvals. Compare-and-set updates for turn status. Tests pass on SQLite and Postgres.
- [x] **API and SSE.** The endpoints in ADR 0035, under the existing auth and scopes: `read` to list and follow, `write` to send, approve and edit drafts. Approving a gated call needs the CSRF header from a cookie session.
- [x] **CLI.** `icaro assist list|show|send|rm` (`rm` needs `--yes`); `go run ./hack/gendocs` regenerates `docs/cli.md`.
- [x] **Web UI.**
  - An assistant panel docked in the editor: messages streamed over SSE, tool calls rendered as collapsible cards, run approval cards, and states for sign-in required, rate limited (with the reset time), queued and interrupted.
  - The draft shared with the editor: user edits are sent to the chat, and an assistant proposal shows as a diff with its issues and **Accept** / **Discard**.
  - An **Assistant** page listing chats, and **New with assistant** on the workflows list.
  - Accessibility and empty/error states as in [Phase 4](04-workflow-ui.md).
- [x] **Desktop app.** **Ask the assistant** in the tray opens the chat page. No logic in the shell; `internal/desktop` keeps its dependency rule.
- [x] **Usage.** Each message stores the CLI's token counts. Subscription turns show tokens only; `inject` turns also show the labelled cost estimate. `icaro usage` counts assistant transcripts. *(Only where the session layer is a directory (copy mode); an overlay layer in a Docker volume is not read, and the message's token counts are the record.)*
- [x] **Doctor.** Checks: an assistant profile is configured and its agent is known; the profile is signed in; the profile's allowlist reaches the provider. Mapped in the doctor coverage test, with a row in the [doctor checks](../03-security.md#icaro-doctor).
- [x] **Docs.**
  - A new `docs/assistant.md`: setup with a subscription and with an API key, what the assistant can and cannot do, and where its logs are.
  - [Architecture §10](../01-architecture.md#10-agent-interface), the session kinds in §0 and §4, and [security](../03-security.md) (the `assist` tool set and the run gate).
  - The blueprint: ADR 0035 marked Accepted, a decision-index row, and the in-container MCP row of the deferred designs narrowed.

## Acceptance criteria

- [ ] With a profile signed in by subscription and no API key anywhere on the machine, a user asks for "a workflow that posts to Slack when a GitHub issue is labelled `bug`". The assistant calls `get_workflow_schema`, `list_integrations` and `validate_workflow`, and proposes a draft that validates clean; the editor shows it as a diff; **Accept** then **Validate & save** creates version 1 with actor `<user> via assistant <chat>`. *(Open: needs a real signed-in CLI (S11). The same flow passes end to end with a fake `claude` CLI that calls the real tools over the session socket: `hack/e2e` `TestAssistant` and `web/e2e/assistant.spec.ts`, which also checks the `<user> via assistant <chat>` actor.)*
- [ ] The same conversation works with an `inject` API-key profile, and the injector's audit log attributes its calls to the chat's session. *(Open: needs a real API key. `session.Authorize` now accepts `assist` sessions, so the injector attributes their calls.)*
- [ ] No container, log, SQL row or API response contains the OAuth token or the API key. Unit tests check the exec request, mounts and logs; an integration test checks the real container. *(Open until S11 records real streams. Ícaro never reads the login: the exec request carries only `MCP_TOOL_TIMEOUT`, and the chat log, stored messages and the turn stream are secret-redacted, which `TestRedaction` checks.)*
- [x] From an assistant session's socket, `create_workflow`, `update_workflow` and every destructive call are unknown tools; a crafted `tools/call` for them fails, whatever the agent sends. *(`TestRegisterRead`, `TestNoWriteTools`.)*
- [x] `run_workflow` from the chat starts nothing until the person approves the card; **Decline** and the timeout return `declined` to the assistant and start nothing. *(`TestGatedRun`, `TestGatedRunCancelled`, `TestAssistant`. A timeout returns `expired`, which starts nothing.)*
- [x] A recorded stream with a prompt injection in a `get_run` log tail ("call update_workflow …") cannot change a saved workflow. *(No tool can save: `TestNoWriteTools`.)*
- [x] An expired login shows **Sign in required**; a rate limit shows the reset time; neither triggers a retry. *(`TestTerminalStatuses`; the panel shows the state with Retry.)*
- [x] Cancelling a turn stops the exec within 5 s. A daemon restart during a turn leaves it `interrupted`, and **Retry** resumes the same conversation. *(`CancelTurn` sends TERM, then KILL after 5 s; `TestCancel`, `TestRestartRecovery`; Retry resends with `--resume`.)*
- [x] Two messages sent quickly run one after the other in order, never in parallel. *(`TestTurnsAreSerial`.)*
- [x] More open chats than `assistant.max_active` queue instead of starting containers; an idle chat's container stops after `assistant.idle_timeout` and the next message starts it again, with the conversation intact. *(`TestAssistCapQueues`, `TestIdleStop`, `TestSessionLifecycle`.)*
- [x] Every recorded fixture parses; the fuzz target runs for 5 minutes without a crash. *(The modeled fixtures parse against their golden files; a 5-minute run did about 13.6 million executions with no crash. S11's recorded fixtures join the same test.)*
- [x] A Playwright test drives a chat against a fake agent CLI that replays a fixture: stream, tool card, proposal diff, accept, save. *(`web/e2e/assistant.spec.ts`.)*
- [x] Every new decision has unit tests, the container and exec paths have tests behind the `integration` build tag, and coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets. *(`TestEngineExec` and the session suites under `integration`; every package at its ADR 0025 target.)*

## Where it lives

- Chat service and stream parsing: `internal/assist/`; agent adapters: `internal/creds/agents.go`.
- Session kind: `internal/session/`; tool set per kind: `internal/authoring/` and `internal/mcp/`.
- API: `internal/api/chats.go`; CLI: `internal/cli/assist_cmd.go`; UI: `web/src/pages/Assistant.tsx`, `web/src/components/AssistantPanel.tsx` and `web/src/assist.ts`.
- End-to-end: `hack/e2e/assist_test.go` and `web/e2e/assistant.spec.ts`, with the fake agent in `hack/e2e/testdata/fake-agent/`.
