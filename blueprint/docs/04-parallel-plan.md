# Parallelism plan

## Critical path: the session spine

The wedge is parallel isolated agents (ADR 0002), so the **critical path is the session spine**, not four independent tracks. The first shippable milestone is a working session end-to-end:

```text
0 (foundation) ── 1 (container + creds broker)
                      │
                      ▼
        SESSION SPINE = worktree + container + preview + memory
                      ▲           ▲
                 5 (previews)  6 (memory/MCP)
```

**Spine milestone (must come before breadth):** create a worktree → run an agent in an isolated container with brokered creds → reach a live preview → read/write scoped memory over MCP. Phases 1, 5, and 6 are sequenced to converge here; they are facets of the spine, not parallel products. Workflow (2/3/4) orchestrates sessions and follows once a single session works.

## Tracks (after the spine exists)

| Track | Phases | Notes |
|---|---|---|
| Spine | 0 → 1 → 5 → 6 | the wedge; converges on a working session |
| Automation | spine → 2 → 3 → 4 | workflows orchestrate sessions; canvas last |
| Platform | spine → 7 → 8 | native app deferred until the spine works; then distribution |
| Product/docs | 0 → 10 | documentation tracks the real APIs |
| Quality | starts at 0 → 11 | gates at every phase |

## Actual dependencies

```text
0 ── 1 ─┬─ 5 ─┬─ SPINE ─┬─ 2 ── 3 ── 4
        └─ 6 ─┘         ├─ 7 ── 8
                        └─ 10

SPINE + 2 + 4 + 7 + 8 + 10 ──> 11
```

## Using simpler models

- **Mechanical tasks:** creating scaffolding, table-driven tests, fixtures, API docs, migrations, repetitive components, and lint fixes. Use a cheap/fast model.
- **Critical tasks:** threat model, concurrency model, Docker security policy, DAG executor, schema evolution, post-crash recovery, and releases. Use a more capable model with mandatory human review.
- **Rule:** the simple model does not decide architecture or change security boundaries; it implements tasks whose acceptance criteria are already locked down.
