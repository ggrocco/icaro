# Phase 6 — Shared memory

**Objective:** cross-agent context with provenance ([architecture §8](../01-architecture.md#8-shared-memory)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Define memory objects and per-project ACL.
- [ ] Implement deterministic ingest of files/symbols/ADRs.
- [ ] Build FTS and retrieval with filters.
- [ ] Add handoff between sessions/CLIs.
- [ ] Expose memory as an **MCP server** injected into each session's agent config over the transport chosen in S3; scope enforced server-side ([ADR 0004](../adr/0004-memory-mcp.md)).
- [ ] Provide a documented HTTP/CLI fallback for non-MCP agents.
- [ ] Prototype Graphify/ai-memory import via adapters, not a fork.

## Acceptance criteria

- [ ] Project A's memory does not leak into project B.
- [ ] Every response includes a source and a timestamp.
- [ ] An MCP-capable agent reads and writes memory with no bespoke integration; an agent cannot reach another project's scope by asking.
- [ ] A handoff can be recovered from another CLI.
- [ ] A 10k-document benchmark holds the defined target latency.
