# Phase 2 — Workflow core

**Objective:** DAG, schema, persistence, execution, and logs.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Version the JSON Schema and validate it in the CLI/API.
- [ ] Add semantic validation for cycles, dependencies, and timeouts.
- [ ] Implement **data flow**: step `outputs`, `env`, `${{ steps.<id>.outputs.<key> }}` interpolation, and `when` gating per the grammar chosen for O3.
- [ ] Implement a DAG planner and an executor with channels, mapping steps onto sessions as decided in O2 ([ADR 0002](../adr/0002-session-spine.md)).
- [ ] Persist run/step state and indexed logs.
- [ ] Implement cancellation, retry, and recovery after restart.

## Acceptance criteria

- [ ] An invalid workflow points to the JSON path and a clear error.
- [ ] A DAG with two independent steps runs in parallel.
- [ ] Step B consumes step A's declared output via interpolation; a reference to a missing output fails validation.
- [ ] Cancellation propagates to child processes.
- [ ] A restart never leaves a run in an ambiguous state.
