# Phase 6 — Shared memory

**Objective:** cross-agent context with provenance ([architecture §8](../01-architecture.md#8-shared-memory)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Define memory objects and per-project ACL.
- [x] Implement deterministic ingest of files/symbols/ADRs.
- [x] Build FTS and retrieval with filters.
- [x] Add handoff between sessions/CLIs.
- [x] Expose memory as an **MCP server** injected into each session's agent config over the transport chosen in S3 ([ADR 0019](../adr/0019-memory-mcp-transport.md)); scope enforced server-side ([ADR 0004](../adr/0004-memory-mcp.md)). The session manager mounts the per-session socket and writes the agent's MCP entry.
- [x] Provide a documented HTTP/CLI fallback for non-MCP agents ([docs/memory.md](../../../docs/memory.md)).
- [x] Prototype Graphify/ai-memory import via adapters, not a fork.

## Acceptance criteria

- [x] Project A's memory does not leak into project B.
- [x] Every response includes a source and a timestamp.
- [x] An MCP-capable agent reads and writes memory with no bespoke integration; an agent cannot reach another project's scope by asking.
- [x] A handoff can be recovered from another CLI.
- [x] A 10k-document benchmark holds the defined target latency (p95 < 50 ms, [ADR 0020](../adr/0020-portable-memory-search.md)).
