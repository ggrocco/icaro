# ADR 0013 — Workflow execution semantics

**Status:** Accepted

## Context

The schema ([workflow contract](../schemas/workflow.schema.json)) fixes the shape of a workflow. It leaves several behaviours to the engine: how an `outputs` entry captures its value, when a step whose dependency failed or was skipped still runs, how retries back off, what happens to a running step when the daemon restarts, and the concurrency default. Phase 2 needs one answer for each, and the CLI, API, UI and MCP tools all show the result.

## Decision

**Output capture.** Each value in an image step's `outputs` map names its source:

| Form | Value |
|---|---|
| `json:<path>` | A value inside `/icaro/output.json`: dotted keys with numeric array indexes (`json:pr.url`, `json:items.0.id`, `json:items[0].id`). `json:$` is the whole document. The value keeps its JSON type. |
| `stdout` | The step's whole stdout, trimmed, capped at 1 MiB |
| `stdout:last-line` | The last non-empty stdout line, trimmed |
| `file:<path>` | A file under `/workspace` (relative, or absolute under `/workspace`), trimmed, capped at 1 MiB. A path that escapes the workspace, including through a symlink, fails the step. |

Validation rejects any other form. A missing JSON path or file fails the step. `/icaro/output.json` must be a JSON object of at most 1 MiB even when no output reads it. Captured values pass through the same redactor as logs.

**Dependencies.** A step runs only when every one of its `needs` either **succeeded** or was **skipped by its own `when`**. If a need failed, was cancelled, or was itself not run, the step is recorded as `skipped` with the error `not run: needs <id> <status>`. That status propagates to its dependents, and the run fails. A step that reads the outputs of a `when`-skipped need gets a missing-key error unless it guards the reference with `has()`. `steps.<id>.status` is always readable.

**`when`** is evaluated once, when all needs are terminal. False skips the step, and an evaluation error fails it.

**Retry and timeout.** `retry: n` allows `n + 1` attempts. The delay before attempt `k + 1` is `2s · 2^(k−1)`, capped at one minute. Each attempt gets the step's `timeout`, or `limits.timeout` when unset. `limits.timeout` is also the ceiling, and validation rejects a larger value. A timed-out container is killed.

**Commands.** An image step's `command` runs as `/bin/sh -c <command>`; without `command`, the image's own entrypoint runs. Step containers run as the session user (`Sessions.User()`: the daemon's uid:gid, or 1000:1000 when the daemon runs as root — containers are never root), which also owns the run workspace and the step's `/icaro` files, so files in the bind-mounted workspace keep the user's ownership.

**Restart.** The engine persists a step's container id before starting the container and labels the container with its run, step and attempt. On startup the engine resumes every `running` run:
- a container that exists and was started is re-attached. Log capture resumes from the last indexed offset, and the step timeout restarts from the moment of re-attach;
- a container that is missing, or was created but never started, fails that attempt, and the retry policy decides what happens next;
- a `uses` step that was in flight is treated as a failed attempt, so actions are **at-least-once** across restarts.

A daemon shutdown never kills step containers; a user cancel always does.

**Concurrency.** An unset `concurrency` means `allow`. `forbid` skips an enqueue while a run of the workflow is queued or running. `queue` lets the run wait in the queue until no run of the workflow is running. `replace` cancels the active runs and then enqueues. An enqueue with an idempotency key that was already used returns the earlier run instead of creating one.

**Manual runs.** The engine computes the `trigger.*` fields a workflow reads outside `has()`; a read on the right of `&&` (or `||`) whose left side has already proven the field present with `has()` counts as guarded, because evaluation short-circuits. A manual run must supply all of them: the API rejects the run with the list of missing fields, and the CLI prompts for them. A cron run carries only `scheduled_at`, `fired_at` and `late`, so validation rejects a workflow with a cron trigger that reads any other field unguarded; its cron runs could never succeed.

**Early failure.** A run that fails or is cancelled before its first step starts (unloadable version, run session, egress, secrets, workflow `env`) still settles every step: `skipped` with `not run: <run error>`, or `cancelled` when the run was cancelled. A cancel during that phase records the run as `cancelled`, not `failed`.

## Consequences

- Run outcomes are deterministic from the step states, and every step ends in a recorded terminal state with a reason. "Why didn't this step run?" is answered by its row.
- Integration actions should be idempotent on their side, or the workflow should gate them with `when`, because a restart can repeat an action.
- Adding a capture form or changing the dependency rule needs a new ADR, because existing workflows depend on both.
