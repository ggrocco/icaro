# ADR 0019 — Memory MCP reaches agents over a per-session socket and a stdio bridge

**Status:** Accepted — resolves feasibility spike [S3](../02-decisions-risks.md#feasibility-gate)

## Context

[ADR 0004](0004-memory-mcp.md) exposes memory over MCP "over the session's Unix socket", but agent CLIs speak MCP over stdio or HTTP, not over a raw socket. The endpoint must reach an agent inside its container with no network route to the daemon, and scope must come from the session, not from the agent.

## Decision

- The daemon serves, **per session**, an MCP endpoint (streamable HTTP, JSON responses) on a `0600` Unix socket. The session manager creates it through `session.MemoryAttacher.ServeSession(identity, path)` and bind-mounts it at `/icaro/mcp.sock`. The identity bound to that socket — project, branch, agent, session — is the only scope its requests can have.
- Inside the container the agent's MCP config launches `icaro mcp --socket /icaro/mcp.sock`, a **stdio bridge** that forwards each JSON-RPC line as an HTTP POST over the socket. It needs no network, no token, and no config file, and works with any MCP client that can launch a command.
- On the host, `icaro mcp` is the same bridge pointed at the daemon's authenticated `/mcp` endpoint, scoped by the project of the directory it runs in (`--project`). The host user may read every project, so the project header is trusted there; it is never trusted inside containers.
- `internal/mcp` is a small, dependency-free server (JSON-RPC 2.0; protocol versions 2025-06-18, 2025-03-26 and 2024-11-05; tools and resources; stdio and streamable HTTP). Every request carries an authenticated `Caller`; tools read scope only from it. Agents in containers see only the three memory tools; the host server is the one that later gains the authoring tools (architecture §10).
- Session socket paths must stay under 104 bytes (macOS `sun_path`); the attacher rejects longer paths, so sessions use short paths such as `~/.icaro/run/s/<short-id>.sock`.

## Consequences

- Any MCP-capable agent uses memory with a one-line config; no per-agent integration.
- A prompt-injected agent cannot read another project's memory: the socket it can reach answers only for its own session.
- The bridge is part of the `icaro` binary already installed in the agent image (Phase 1); there is no separate shim.
- The candidate design in S3 (a loopback port forwarded to the socket) is not needed; it remains an option for clients that only accept an HTTP URL.
