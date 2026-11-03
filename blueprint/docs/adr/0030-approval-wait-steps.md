# ADR 0030 — Approval and wait steps

**Status:** Accepted. Partly lifts [MVP cut 8](../02-decisions-risks.md#mandatory-mvp-cuts) for two step forms; waiting for an event stays deferred.

## Context

Cut 8 limited a step to a container image or an integration action. The deferred `wait` / `approval` design was the gap users hit first: a workflow that deploys, posts to a channel or merges should be able to stop for a person, and a canary should be able to soak for a while before the next step. The obvious way to build either, a step that sleeps in a goroutine or a container, would hold a `workflows.max_runs` slot (and a step slot) for hours, so a few pending approvals could stall every other workflow.

## Decision

**Two new step forms, run by the engine itself.** A step has exactly one of `image`, `uses`, `approval` or `wait`.

- `approval: {message}` waits for a person to approve or reject it. The message may use `${{ }}` over `trigger`, `env`, `run`, `workflow` and the step's `needs` ancestors' outputs, but not `secrets`: it is shown to whoever can read runs. The step's `timeout` is its decision deadline and is exempt from the `limits.timeout` ceiling; without one it waits until decided or cancelled. It outputs `approved_by` and `comment`.
- `wait: <duration>` waits from the step's start for `s`/`m`/`h` units, at most `720h`. It has no outputs and no `timeout`.
- Neither takes `command`, `env`, `input`, `outputs`, `repository` or `retry`.

**Parking.** Pause steps run no goroutine and no container. The scheduler starts them (status `waiting`, `started_at` recorded) and checks them on every pass, so other branches of the DAG keep running. When only waiting steps are left, the run is **parked**: its status becomes `waiting`, `runs.wake_at` is set to the earliest wait end or approval deadline (none when only an undecided approval without a deadline remains), and its engine slot is released. The engine's claim loop takes queued runs first, then parked runs that are due: `wake_at <= now`, or a cancel request. A claimed parked run resumes from its stored steps as a restart does ([ADR 0013](0013-workflow-execution-semantics.md)), so a wait survives a daemon restart.

**Decisions.** `step_approvals` holds one row per approval step of a run: the rendered message, then the decision (`approved`, `rejected`, `expired`), the actor, a comment (at most 2000 bytes) and the time. A decision is recorded only while the row is undecided, and in the same transaction sets the run's `wake_at` to now when it is parked; it is final. An undecided approval past its deadline is recorded as `expired` by the engine, and a decision that lands first wins. `POST /api/v1/runs/{id}/steps/{step}/approve` and `/reject` (body `{"comment"}`) need the `write` scope and record the caller (`token:<name>`) as the approver; `icaro runs approve` / `reject` and the run page in the web UI call them. A rejection needs a comment in the CLI and the UI. A rejected or expired approval fails the step, and the steps that need it are skipped as for any failed need.

**Only people decide.** No MCP tool approves or rejects; `get_run` shows the request and the command a person runs. Step and session containers reach only their session's memory socket, never the daemon API, so a run cannot approve itself.

**Status and policies.** `waiting` is a run status and a step status. A waiting run is active: `forbid`, `queue` and `replace` treat it like a running one, and `replace`, cancel and workflow delete wake it so the engine cancels its waiting steps (and records the run `cancelled`) rather than marking it from the outside.

**Not now:** waiting until an event arrives (an integration webhook resuming a run). It needs a correlation key from the event to the run and a decision on what a stray or late event does. It stays in the [deferred designs](../02-decisions-risks.md#deferred-designs).

## Consequences

- A pending approval costs one database row and no slot, so `max_runs` bounds work in progress, not decisions outstanding.
- A parked run is resumed at most one poll interval (`workflows` engine poll, 1s by default) after it is due; a decision or a cancel wakes the engine at once.
- `waiting` is a new status for API clients. The CLI, the web UI and the desktop tray list waiting runs among the active ones.
- Approvals are as trustworthy as the tokens that can write: a `write` token used by automation can approve. Scope tokens accordingly.
