# ADR 0031 — A concurrency cap on agent sessions, derived from RAM

**Status:** Accepted. Resolves [open item O1](../02-decisions-risks.md#open-items) (compute budget for N parallel sessions). Records what the session manager has done since the spine milestone.

## Context

The wedge is many agents in parallel ([ADR 0002](0002-session-spine.md)), but each session costs an agent container **plus** a preview compose stack. On a laptop, Docker Desktop's fixed VM memory ceiling is hit after a handful of sessions, and when it is, containers are OOM-killed at random rather than refused up front. O1 proposed a configurable cap with a RAM-derived default and per-container limits; the code adopted that proposal, but no ADR recorded it.

## Decision

**A cap on running agent sessions.** At most `sessions.max_active` agent sessions are `creating` or `running` at once. `0` (the default) derives the cap from physical RAM: one session per 4 GiB, at least 1, at most 1024; when RAM cannot be read the cap is 2. RAM is read from `/proc/meminfo` on Linux and `sysctl hw.memsize` on macOS.

**Over the cap, a session is queued, not refused.** `session create` and `session start` persist the session as `queued`. The manager's loop is the only place that claims queued sessions: it counts the active ones, and while below the cap moves queued sessions to `creating`, oldest first, with a compare-and-set update, so a concurrent stop or a second claim cannot double-start one. A session whose profile is being re-seeded by `icaro login` stays queued until the login ends. The loop fills again whenever a start finishes, a session stops, or its tick fires, so a freed slot is used within one tick. Because `queued` is stored, the queue survives a daemon restart, and the startup reconciliation ([ADR 0006](0006-session-persistence-recovery.md)) leaves it in place.

**Per-container limits.** Every container Ícaro creates is bounded by `limits` (`memory_mb` 2048, `cpus` 2, `pids` 512, `nofile` 4096 by default). Nothing in a workflow can raise them.

**Scope.** The cap counts agent sessions only. Workflow run sessions ([ADR 0010](0010-run-sessions.md)) are bounded by their own knobs, `workflows.max_runs` and `workflows.concurrency`, so automation does not starve interactive sessions and the other way round.

## Consequences

- Creating more sessions than the machine can hold degrades into a wait, not an OOM kill. `queued` is a session status the API and `icaro session` commands report, and `session stop`/`rm` work on a queued session.
- The 4 GiB-per-session budget is a heuristic for an agent container plus a typical preview stack. It reads host RAM, not the Docker VM's memory, so on Docker Desktop with a small VM the derived cap can be too high; set `sessions.max_active` explicitly there.
- Preview compose stacks are not bounded by `limits`: the project's compose file sets their resources. Only the per-session budget accounts for them.
- Agent and run sessions draw from separate caps, so their sum can exceed what either cap alone suggests; the per-container limits bound the worst case.
- A polling file watcher costs CPU per active session ([residual risks](../02-decisions-risks.md#residual-risks)); the cap bounds that too.
