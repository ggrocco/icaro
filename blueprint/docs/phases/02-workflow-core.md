# Phase 2 — Workflow core

## Objective

Objective: DAG, schema, persistence, execution, and logs.

## Dependencies

Can start in parallel with Phase 1 once the config contract is in place.

## Deliverables

- [ ] Version the JSON Schema and validate it in the CLI/API.
- [ ] Add semantic validation for cycles, dependencies, and timeouts.
- [ ] Implement **data flow**: step `outputs`, `env`, and `${{ steps.<id>.outputs.<key> }}` interpolation, with a defined `when` expression grammar for step gating.
- [ ] Implement a DAG planner and an executor with channels; the unit of work a step drives is a **session** (ADR 0002), not a generic job.
- [ ] Persist run/step state and indexed logs.
- [ ] Implement cancellation, retry, and recovery after restart.

## Acceptance criteria & manual tests

- [ ] An invalid workflow points to the JSON path and a clear error.
- [ ] A DAG with two independent steps runs in parallel.
- [ ] Step B consumes step A's declared output via interpolation; a reference to a missing output fails validation.
- [ ] Cancellation propagates to child processes.
- [ ] A restart never leaves a run in an ambiguous state.

## Automated tests

- Unit: business logic and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: only critical flows, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not advance until the criteria above are green. Record new decisions in `docs/adr/`.
