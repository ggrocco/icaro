# Phase 0 — Foundation & contracts

**Objective:** repository, CI, conventions, daemon lock, configuration, and a minimal doctor.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Set up a Go + web monorepo and a Makefile.
- [x] Define small interfaces and ownership per goroutine/channel. *(The store is the exception: a `database/sql` repository whose shared state is guarded by compare-and-set updates and transactions; see [architecture §4](../01-architecture.md#4-persistence).)*
- [x] Implement versioned config and migrations.
- [x] Implement lock/socket/PID and clean shutdown.
- [x] Add CI: fmt, vet, staticcheck, tests, coverage, and cross-platform build.
- [x] Migrate the [existing-code](../04-roadmap.md#existing-code) rows assigned to this phase.

## Acceptance criteria

- [x] `icaro version` works on macOS/Linux.
- [x] A duplicate `icaro serve` fails deterministically.
- [x] Killing the process and running `icaro doctor` detects and cleans up orphaned state.
- [x] Domain coverage meets the [target](../05-engineering.md#coverage); exceptions justified. *(CI gates every `internal/*` package at 95%; the two exceptions, `cli` and the Docker adapter, are justified in [ADR 0025](../adr/0025-coverage-target-and-exceptions.md).)*
