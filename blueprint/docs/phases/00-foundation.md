# Phase 0 — Foundation & contracts

## Objective

Objective: repository, CI, conventions, daemon lock, configuration, and a minimal doctor.

## Dependencies

Blocks all phases.

## Deliverables

- [ ] Set up a Go + web monorepo and a Makefile.
- [ ] Define small interfaces and ownership per goroutine/channel.
- [ ] Implement versioned config and migrations.
- [ ] Implement lock/socket/PID and clean shutdown.
- [ ] Add CI: fmt, vet, staticcheck, tests, coverage, and cross-platform build.

## Acceptance criteria & manual tests

- [ ] `icaro version` works on macOS/Linux.
- [ ] A duplicate `icaro serve` fails deterministically.
- [ ] Killing the process and running `icaro doctor` detects and cleans up orphaned state.
- [ ] Domain coverage >= 95%; exceptions justified.

## Automated tests

- Unit: business logic and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: only critical flows, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not advance until the criteria above are green. Record new decisions in `docs/adr/`.
