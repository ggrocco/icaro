# Phase 4 — Workflow UI

## Objective

Objective: React editor and observability.

## Dependencies

Depends on Phase 2. The canvas is a later subphase.

## Deliverables

- [ ] Create a listing, a details view, and an SSE log stream.
- [ ] Add the form editor first; drag/drop canvas later.
- [ ] Generate canonical JSON and validate before saving.
- [ ] Display the DAG, status, and attempts per step.
- [ ] Add accessibility and error/empty states.

## Acceptance criteria & manual tests

- [ ] Editing in the form produces compatible JSON.
- [ ] A canvas round-trip does not lose unknown fields.
- [ ] The UI updates status without aggressive polling.
- [ ] An E2E test covers create, run, and cancel.

## Automated tests

- Unit: business logic and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: only critical flows, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not advance until the criteria above are green. Record new decisions in `docs/adr/`.
