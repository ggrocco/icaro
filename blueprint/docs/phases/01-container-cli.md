# Phase 1 — Container and CLI

## Objective

Objective: isolated profile and the `setup`, `sh`, and `claude` commands.

## Dependencies

Can start after Phase 0.

## Deliverables

- [ ] Create a non-root base Dockerfile with asdf.
- [ ] Implement image-by-digest, profile, and mount policy.
- [ ] Implement an idempotent setup with diff preview and overwrite confirmation.
- [ ] Implement `icaro sh`, `icaro claude`, the CLI adapter, and TTY passthrough.
- [ ] Install Ícaro inside the image for internal commands.
- [ ] Implement the **credential broker** (ADR 0003): scoped/short-lived access over the session socket; no raw creds mount by default.
- [ ] Stand up `internal/session/` and **worktree lifecycle**; an agent runs against a session's worktree (this is the spine's first segment, ADR 0002).
- [ ] Spike per-container microVM isolation (Apple Containerization / gVisor) and record the result in an ADR.

## Acceptance criteria & manual tests

- [ ] Running `icaro setup` twice changes nothing without consent.
- [ ] A file outside the allowlist is not mounted.
- [ ] `icaro sh` preserves the UID and the expected workspace.
- [ ] The CLI agent receives a TTY and the correct exit code.
- [ ] An agent obtains provider access via the broker with **no** `~/.claude`/`~/.config/gh` bind mount.
- [ ] Two sessions on two worktrees run concurrently without interfering.

## Automated tests

- Unit: business logic and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: only critical flows, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not advance until the criteria above are green. Record new decisions in `docs/adr/`.
