# Phase 2 — Workflow core

**Objective:** DAG, schema, persistence, execution, and logs.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Version the JSON Schema and validate it in the CLI/API; generate it from the engine's types with a drift test against the [blueprint contract](../schemas/workflow.schema.json).
- [x] Implement the [image-step I/O contract](../01-architecture.md#6-workflow-engine) (`/icaro/input.json`, `/icaro/output.json` capped at 1 MiB, `/workspace`, exit code).
- [x] Add semantic validation for cycles, dependencies, and timeouts.
- [x] Implement **data flow**: step `outputs`, `env`, `${{ steps.<id>.outputs.<key> }}` interpolation, and `when` gating per the grammar chosen for O3.
- [x] Implement a DAG planner and an executor with channels, mapping steps onto sessions as decided in O2 ([ADR 0002](../adr/0002-session-spine.md)).
- [x] Persist run/step state and indexed logs.
- [x] Implement cancellation, retry, and recovery after restart.
- [x] Migrate the [existing-code](../04-roadmap.md#existing-code) rows assigned to this phase.

## Acceptance criteria

- [x] An invalid workflow returns structured issues: JSON Pointer path, clear message, and a nearest-match hint for unknown names.
- [x] A DAG with two independent steps runs in parallel.
- [x] Step B consumes step A's declared output via interpolation; a reference to a missing output fails validation.
- [x] Cancellation propagates to child processes.
- [x] A restart never leaves a run in an ambiguous state.

## Status

Implemented in `internal/workflow` (spec, schema generation, validation, plan, capture), `internal/workflow/engine` (executor), `internal/workflow/events` (SSE fan-out), `internal/service` and migration `00006`. Execution semantics are [ADR 0013](../adr/0013-workflow-execution-semantics.md). Every acceptance criterion has a test: unit tests against the fake engine in `internal/workflow/engine/engine_test.go`, and real-Docker tests behind the `integration` build tag (parallel overlap, cancel kills the container, re-attach after a daemon restart).
