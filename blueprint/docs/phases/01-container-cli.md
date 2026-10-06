# Phase 1 — Container and CLI

**Objective:** isolated profile, the session's first segment, and the `setup`, `sh`, and `claude` commands.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Create a non-root base Dockerfile with asdf.
- [ ] Implement image-by-digest, profile, and mount policy.
- [ ] Implement an idempotent setup with diff preview and overwrite confirmation.
- [ ] Implement `icaro sh`, `icaro claude`, the CLI adapter, and TTY passthrough.
- [ ] Install Ícaro inside the image for internal commands.
- [ ] Implement the **per-profile persistent home** ([ADR 0005](../adr/0005-persistent-home-egress.md)): login seeding, golden seed layer + per-session writable overlay, and the mandatory egress allowlist locked to provider endpoints.
- [ ] Stand up `internal/session/` and **worktree lifecycle**; an agent runs against a session's worktree ([ADR 0002](../adr/0002-session-spine.md)).
- [ ] Persist sessions in the `sessions` table and reconcile them on startup ([ADR 0006](../adr/0006-session-persistence-recovery.md)).
- [ ] Migrate the [existing-code](../04-roadmap.md#existing-code) rows assigned to this phase.

## Acceptance criteria

- [ ] Running `icaro setup` twice changes nothing without consent.
- [ ] A file outside the allowlist is not mounted.
- [ ] `icaro sh` preserves the UID and the expected workspace on both macOS and Linux.
- [ ] The CLI agent receives a TTY and the correct exit code.
- [ ] The egress allowlist confines every credential present in the agent container: a request to a non-allowlisted host fails. The host's real `~/.claude` is never mounted.
- [ ] Two sessions sharing a profile use the seeded login concurrently without corrupting each other's `~/.claude`.
- [ ] Two sessions on two worktrees run concurrently without interfering.
- [ ] After a daemon restart, a running session is re-attached or marked `orphaned`, never left ambiguous.
