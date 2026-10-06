# Phase 11 — Hardening and GA

**Objective:** reliability, performance, and security before GA.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Load tests for workflows and SSE.
- [ ] Chaos tests: Docker restarts, daemon crashes, DB lock.
- [ ] Dependency/license audit.
- [ ] Performance profiling and memory/CPU budget.
- [ ] Compatibility matrix and public beta.

## Acceptance criteria

- [ ] SLOs met on the reference hardware.
- [ ] No deadlock/race under `-race` where applicable.
- [ ] Doctor resolves every scenario in the [doctor checks](../03-security.md#icaro-doctor).
- [ ] The release candidate passes smoke tests on macOS/Linux.
