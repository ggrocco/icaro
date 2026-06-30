# Phase 11 — Hardening and GA

## Objective

Objective: reliability, performance, and security before GA.

## Dependencies

Final phase.

## Deliverables

- [ ] Load tests for workflows and SSE.
- [ ] Chaos tests: Docker restarts, daemon crashes, DB lock.
- [ ] Dependency/license audit.
- [ ] Performance profiling and memory/CPU budget.
- [ ] Compatibility matrix and public beta.

## Acceptance criteria & manual tests

- [ ] SLOs met on the reference hardware.
- [ ] No deadlock/race under `-race` where applicable.
- [ ] Doctor resolves the defined scenarios.
- [ ] The release candidate passes smoke tests on macOS/Linux.

## Automated tests

- Unit: business rules and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: critical flows only, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not move forward until the criteria above are green. Record any new decisions in `docs/adr/`.
