# Ícaro — Product & Engineering Blueprint

Ícaro is a local-first platform for running **many coding agents in parallel** — each on its own git worktree, isolated in a container, exposed at a predictable preview URL, and sharing project memory. Workflow automation orchestrates those agents.

## Primary wedge

The four capabilities (container isolation, previews, shared memory, workflow automation) are **not** four parallel products. They unify around one primitive — the **session** ([ADR 0002](docs/adr/0002-session-spine.md)):

> create a **worktree** → spin an isolated **container** running the agent → expose a **live preview** at a predictable URL → record decisions/handoffs to **shared memory** → optionally orchestrate with a **workflow**.

The MVP is the parallel-isolated-agents experience built on this spine. Anything that does not make a session more useful is deferred.

## Personas

- An individual developer who switches between Claude Code/Codex/Pi and needs a reproducible environment.
- A tech lead who automates review, migration, and testing through local workflows/webhooks.
- A developer who uses git worktrees and wants stable local previews.

## Jobs to be done

1. "When I start a project on another machine, I want a reproducible agent environment without polluting my host."
2. "When a branch/worktree appears, I want to see the application at a predictable URL without configuring a proxy manually."
3. "When several agents work on the same project, I want to preserve decisions and context from a verifiable source."
4. "When a repetitive routine occurs, I want an observable, cancelable, and auditable workflow."

## Success metrics

- Initial setup < 10 min on a machine with Docker ready.
- Starting an isolated CLI < 3 s once the image is available.
- A simple workflow with streaming logs and correct status.
- A preview detected and routed in < 10 s after a valid compose.
- Zero duplicate processes and zero orphan previews in the test scenarios.

## Principles

Each principle is a one-line summary; the linked doc is canonical.

- **One binary, one daemon.** CLI, HTTP server, and the macOS app are clients of the same per-user daemon ([ADR 0001](docs/adr/0001-single-daemon.md)); Linux gets CLI + daemon only in the MVP.
- **Local-first state** in `~/.icaro/` (paths configurable), stored in SQLite behind a `database/sql` store ([architecture §4](docs/01-architecture.md#4-persistence)).
- **Secure by default.** Non-privileged containers, no Docker socket, an explicit mount allowlist, and a mandatory egress allowlist ([security](docs/03-security.md)).
- **Credentials live in a managed, per-profile home** that is seeded once and confined by the egress allowlist. The host's real `~/.claude` is never mounted ([ADR 0005](docs/adr/0005-persistent-home-egress.md)).
- **Memory speaks MCP**, so any MCP-capable agent CLI uses it without bespoke integration ([ADR 0004](docs/adr/0004-memory-mcp.md)).
- **Integrations are data.** Triggers and external-API actions are YAML manifests in git-backed packs, so adding one does not need a release ([ADR 0007](docs/adr/0007-integration-manifests.md)).
- **Compose, don't reimplement.** The agent CLI is the intelligence; Ícaro provides launch, confinement, and plumbing.
- **Agents are first-class users.** They author and run workflows through MCP tools validated against the published schema ([architecture §10](docs/01-architecture.md#10-agent-interface)).
- **Channels, not mutexes.** Each goroutine owns its state and communicates over channels; no mutex in domain code ([engineering](docs/05-engineering.md)).

## Documents

| Doc | Holds |
|---|---|
| [01 Architecture](docs/01-architecture.md) | The current design: processes, modules, persistence, container, workflow, previews, memory, integrations |
| [02 Decisions & risks](docs/02-decisions-risks.md) | Decision index, MVP cuts, open items, feasibility gate, residual risks |
| [03 Security & operations](docs/03-security.md) | Threat model, mandatory policies, `icaro doctor` checks |
| [04 Roadmap](docs/04-roadmap.md) | Critical path, phase dependencies, rules every phase follows; per-phase packets in [`docs/phases/`](docs/phases/) |
| [05 Engineering standards](docs/05-engineering.md) | Test strategy and dependency policy |
| [Workflow contract](docs/schemas/workflow.schema.json) | JSON Schema for workflow files |
| ADRs | [0001 single daemon](docs/adr/0001-single-daemon.md) · [0002 session spine](docs/adr/0002-session-spine.md) · [0003 credential broker (superseded)](docs/adr/0003-credential-broker.md) · [0004 memory over MCP](docs/adr/0004-memory-mcp.md) · [0005 persistent home + egress](docs/adr/0005-persistent-home-egress.md) · [0006 session persistence](docs/adr/0006-session-persistence-recovery.md) · [0007 integration manifests](docs/adr/0007-integration-manifests.md) · [0008 blueprint supersedes the engine design](docs/adr/0008-blueprint-supersedes-engine-design.md) |

How the docs divide the work: an **ADR** records why a decision was made, the **architecture** describes the current design, **security** holds the policies, and **phases** hold deliverables and acceptance tests. Each fact lives in one of them, and the others link to it.
