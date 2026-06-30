# ADR 0002 — The session is the core primitive

**Status:** Accepted

## Context

The blueprint described four capabilities — container isolation, per-worktree previews, shared memory, and workflow automation — as parallel tracks. Each overlaps an existing tool (`container-use`, Tilt/Coder/Daytona, the AI-memory ecosystem, `act`/Dagger). Built independently they risk becoming four mediocre features with no single reason to install Ícaro.

## Decision

A **session** is the unifying primitive and the MVP wedge. A session is one unit of agent work and owns its lifecycle:

```text
worktree → container → agent → preview → memory  (optionally orchestrated by a workflow)
```

- A new package `internal/session/` owns the lifecycle and is the parent abstraction for container, preview, and memory. Those packages expose capabilities; `session` composes them.
- `internal/session/` owns **git worktree lifecycle** (create, track, clean up). No other component creates worktrees. This closes the gap where "per-worktree" appeared in two pillars with no owner.
- The product story is **N coding agents running in parallel**, each in its own session: isolated, individually previewable, sharing project memory.
- Previews, memory, and workflows are **facets of a session**, not standalone tracks. A workflow orchestrates sessions; it is not a general-purpose CI engine (see ADR-scoped workflow contract in `docs/schemas/workflow.schema.json`).

## Consequences

- The roadmap sequences around the spine: container + worktree + preview + memory must interoperate through `session` before breadth features (canvas UI, native app, distribution) are worth building.
- Every feature is justified by "does this make a session more useful?" Anything that does not is deferred.
- The hardest integration — keeping a preview live while the agent edits code **inside** its container — is a first-class session concern, not an afterthought (see `docs/01-architecture.md` §7).
