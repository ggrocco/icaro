# Phase 6 — Shared memory

## Objective

Objective: cross-agent context with provenance.

## Dependencies

Can start after Phase 0 and evolve in parallel with previews.

## Deliverables

- [ ] Define memory objects and per-project ACL.
- [ ] Implement deterministic ingest of files/symbols/ADRs.
- [ ] Build FTS and retrieval with filters.
- [ ] Add handoff between sessions/CLIs.
- [ ] Expose memory as an **MCP server** injected into each session's agent config; scope enforced server-side (ADR 0004).
- [ ] Provide a documented HTTP/CLI fallback for non-MCP agents.
- [ ] Prototype Graphify/ai-memory import via adapters, not a fork.

## Acceptance criteria & manual tests

- [ ] Project A's memory does not leak into project B.
- [ ] Every response includes a source and a timestamp.
- [ ] An MCP-capable agent reads and writes memory with no bespoke integration; an agent cannot reach another project's scope by asking.
- [ ] A handoff can be recovered from another CLI.
- [ ] A 10k-document benchmark holds the defined target latency.

## Automated tests

- Unit: business rules and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: critical flows only, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not move forward until the criteria above are green. Record any new decisions in `docs/adr/`.
