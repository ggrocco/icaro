# Phase 5 — Previews per worktree

## Objective

Objective: watcher, Compose, proxy, and cleanup.

## Dependencies

Can start partially after Phase 0; full integration depends on the container policy.

## Deliverables

- [ ] Create a worktree scanner/watch with debounce.
- [ ] Implement Compose linting and a safe policy.
- [ ] Create Traefik label overrides and a dedicated network.
- [ ] Bring compose up/down per worktree and record state.
- [ ] Resolve optional `.test` and a later DNS-01 domain/TLS.
- [ ] Wire the **container↔preview seam** through `session`: the agent's `/workspace` and the preview's build context resolve to the same worktree, so agent edits reach the preview (ADR 0002 / architecture §7).
- [ ] Spike file-event propagation across the container boundary on macOS (Docker Desktop file sharing) and record results.

## Acceptance criteria & manual tests

- [ ] Creating a worktree with a compose file brings up a preview.
- [ ] Removing a worktree runs an idempotent cleanup.
- [ ] Two worktrees with the same slug do not collide.
- [ ] A dangerous compose file is blocked and explains why.
- [ ] An agent editing code inside its container produces a visible change in the preview (compose project with a declared port).

## Automated tests

- Unit: business logic and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: only critical flows, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not advance until the criteria above are green. Record new decisions in `docs/adr/`.
