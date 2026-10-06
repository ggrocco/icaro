# ADR 0006 — Session persistence & recovery

**Status:** Accepted

## Context

[ADR 0002](0002-session-spine.md) makes the **session** the core primitive, but
the initial table list in `docs/01-architecture.md` §4 persists the *facets*
(`preview_instances`, `runs`, `memories`) and not the *parent*. There is no
`sessions` table. This collides with two stated requirements:

- Phase 2: "a restart never leaves a run in an ambiguous state";
- Phase 8: live auto-update of a daemon that is actively supervising containers.

Recovering live session state after a crash, restart, or in-place upgrade has no
data model behind it.

## Decision

The session is **first-class in SQLite**, and the daemon **reconciles** live
state on startup.

- Add a **`sessions`** table:
  `id`, `project_id`, `profile`, `worktree_path`, `container_id`,
  `preview_instance_id`, `status`, `created_at`, `updated_at` (and an optional
  `last_heartbeat`). `status` ∈
  `creating | running | queued | stopped | orphaned | error`.
  `preview_instances` and the relevant `runs`/`step_runs` reference `sessions`.
- Writes go through the **single store goroutine** and command channel
  (consistent with architecture §4); no mutex in domain code.
- **Startup reconciliation:** on boot, for each non-terminal session, probe
  whether its container / worktree / preview still exist. Re-attach where
  possible; otherwise mark `orphaned` and surface it via `icaro doctor`.
- **Queued state:** the `queued` status is the persistence hook for the
  concurrency cap (see `docs/08-risks-feasibility.md`); sessions over the cap are
  persisted as `queued` rather than started.
- **Upgrade handoff (Phase 8):** the daemon persists enough to **re-attach** to
  running containers after a restart instead of orphaning them.

## Consequences

- Closes the Phase 2 "no ambiguous state after restart" criterion and the
  Phase 8 live-upgrade gap with a concrete model.
- `sessions` becomes the parent the spine already implies in code (ADR 0002) but
  the schema had not yet expressed.
- Add `sessions` to the initial-tables list in architecture §4 the next time
  that file is edited.
