# Ícaro — Product & Engineering Blueprint

Ícaro is a local-first platform for running **many coding agents in parallel** — each on its own git worktree, isolated in a container, exposed at a predictable preview URL, and sharing project memory. Workflow automation orchestrates those agents.

## Primary wedge

The four capabilities (container isolation, previews, shared memory, workflow automation) are **not** four parallel products. They unify around one primitive — the **session** (see [ADR 0002](docs/adr/0002-session-spine.md)):

> create a **worktree** → spin an isolated **container** running the agent → expose a **live preview** at a predictable URL → record decisions/handoffs to **shared memory** → optionally orchestrate with a **workflow**.

The MVP wedge is the parallel-isolated-agents experience built on this spine. Previews, memory, and workflows are facets of a session, owned by `internal/session/`, not independent tracks. Anything that does not serve the session spine is deferred.

## Consolidated assumptions

- A single Go binary with three surfaces: CLI, HTTP server, and a macOS app via Wails + embedded React.
- macOS: the native app, menu bar, and local server are modes of the **same daemon**, never two competing processes.
- Linux: CLI + HTTP daemon; no desktop app in the MVP.
- Configuration and local state in `~/.icaro/`, with configurable paths.
- Security first: non-privileged containers, the Docker socket never mounted inside agent containers, an explicit allowlist of mounts, and minimal capabilities.
- Concurrency: per-goroutine ownership and communication over channels; no mutex in domain code.
- Database: local-first SQLite (pure-Go, CGO-free driver) behind a `database/sql` store interface, so it can point at any SQL-compatible database if needed.
- Credentials are brokered, never raw-mounted: agents receive scoped, short-lived access rather than a bind mount of `~/.claude` and friends (see [ADR 0003](docs/adr/0003-credential-broker.md)).
- Shared memory is exposed to agents as an **MCP server**, so any MCP-capable CLI reads and writes it without bespoke integration (see [ADR 0004](docs/adr/0004-memory-mcp.md)).

## Artifacts

- [Architecture](docs/01-architecture.md)
- [Decisions & risks](docs/02-decisions-risks.md)
- [Operational model & security](docs/03-security.md)
- [Risks & feasibility gate](docs/08-risks-feasibility.md)
- [Workflow contract](docs/schemas/workflow.schema.json)
- [Phased roadmap](docs/phases/)
- ADRs: [0001 single daemon](docs/adr/0001-single-daemon.md) · [0002 session spine](docs/adr/0002-session-spine.md) · [0003 credential broker](docs/adr/0003-credential-broker.md) · [0004 memory over MCP](docs/adr/0004-memory-mcp.md) · [0005 persistent home + egress](docs/adr/0005-persistent-home-egress.md) · [0006 session persistence](docs/adr/0006-session-persistence-recovery.md)

![Ícaro AI development overview](assets/icaro-overview-en.png)
