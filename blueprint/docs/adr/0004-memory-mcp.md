# ADR 0004 — Shared memory is exposed over MCP

**Status:** Accepted

## Context

Shared memory is potentially the most valuable pillar, but the original plan left the central question unanswered: **how does a real agent CLI read and write memory during a session?** Without an interface the agents already speak, memory is a database nobody writes to. Claude Code, Codex, and Copilot CLI all support the Model Context Protocol (MCP).

## Decision

The memory engine is exposed to agents as an **MCP server** hosted by the daemon.

- Each session injects an MCP server endpoint (over the session's Unix socket) into the agent's configuration during setup.
- Tools exposed: write a memory (decision/task/finding/handoff), query memory (FTS + filters), and read provenance. Every result carries source, timestamp, and scope.
- Scope is enforced server-side from the session's identity (`project_id`, branch/worktree, agent, origin) — the agent cannot read another project's memory by asking.
- Deterministic ingest (files, symbols, imports, commits, ADRs) populates memory without agent involvement; the MCP surface is for human/agent memory and retrieval.

## Consequences

- "How does the agent interact with memory?" is answered by a standard the target CLIs already implement — minimal per-agent integration, immediate value, a differentiator.
- The same MCP surface can later expose session/preview/workflow controls to agents.
- Non-MCP agents fall back to a documented HTTP/CLI interface; MCP is the primary path, not the only one.
- Hybrid retrieval stays as planned: FTS first, vectors only after a local benchmark justifies them.
