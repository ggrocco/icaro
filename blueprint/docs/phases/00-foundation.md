# Phase 0 — Foundation & contracts

**Objective:** repository, CI, conventions, daemon lock, configuration, and a minimal doctor.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Set up a Go + web monorepo and a Makefile.
- [ ] Define small interfaces and ownership per goroutine/channel.
- [ ] Implement versioned config and migrations.
- [ ] Implement lock/socket/PID and clean shutdown.
- [ ] Add CI: fmt, vet, staticcheck, tests, coverage, and cross-platform build.
- [ ] Migrate the [existing-code](../04-roadmap.md#existing-code) rows assigned to this phase.

## Acceptance criteria

- [ ] `icaro version` works on macOS/Linux.
- [ ] A duplicate `icaro serve` fails deterministically.
- [ ] Killing the process and running `icaro doctor` detects and cleans up orphaned state.
- [ ] Domain coverage meets the [target](../05-engineering.md#coverage); exceptions justified.
