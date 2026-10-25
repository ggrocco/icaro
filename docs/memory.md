# Shared memory

Ícaro keeps one memory per **project**: decisions, tasks, findings and handoffs written by agents and humans, plus facts indexed from the repository (files, symbols, imports, ADRs, commits). Design: [architecture §8](../blueprint/docs/01-architecture.md#8-shared-memory), [ADR 0004](../blueprint/docs/adr/0004-memory-mcp.md), [ADR 0019](../blueprint/docs/adr/0019-memory-mcp-transport.md), [ADR 0020](../blueprint/docs/adr/0020-portable-memory-search.md).

## Scope and provenance

- A **project id** names a git repository: `p_` + 16 hex characters of the SHA-256 of the repository's main worktree path (symlinks resolved). Every worktree of a repository shares its project, so parallel sessions share memory. A directory outside git is its own project. `icaro memory project [PATH]` prints the id; Go code uses `memory.ProjectIDForPath`.
- Every read and write is bound to one project, taken from the caller's identity: the session for agents in containers, the `--project` directory for host CLIs. Tool arguments never choose the project; an MCP call that passes `project_id` is rejected.
- Writes are stamped with branch, worktree, agent, session, origin (`agent`, `cli`, `api`, `ingest`, `import:<format>`) and time. Every memory returned carries `source`, `link`, `created_at` and `scope`.
- A memory may have a TTL (`ttl_seconds`, `--ttl`); expired memories are hidden at once and purged by the daemon. `icaro doctor` reports and purges stragglers.

## Kinds

| Kind | Written by | Notes |
|---|---|---|
| `decision`, `task`, `finding` | agents, humans | free text with a title and body |
| `handoff` | agents, humans | `pending` until accepted; accepting records who and when |
| `file`, `symbol`, `adr`, `commit` | deterministic ingest | never written by agents |

## MCP (primary path)

The daemon serves three tools — `memory_write`, `memory_query`, `memory_provenance` — over MCP.

- **Inside a session container** the agent's MCP config runs the stdio bridge against the session socket mounted at `/icaro/mcp.sock`:

  ```json
  {"mcpServers": {"icaro": {"command": "icaro", "args": ["mcp", "--socket", "/icaro/mcp.sock"]}}}
  ```

  The socket is created by the daemon per session (`0600`) at `~/.icaro/run/sessions/<id>/mcp.sock`; its scope is the session's identity. If `ICARO_HOME` is so long that this path exceeds the 103-byte Unix socket limit, sessions start without memory and `icaro doctor` warns about it.
- **On the host** run `icaro mcp` (optionally `--project <dir>`, `--agent <name>`) as a stdio MCP server; it bridges to the daemon's `/mcp` endpoint with the project of the current repository:

  ```sh
  claude mcp add icaro -- icaro mcp
  ```

- Streamable HTTP clients can talk to `/mcp` directly with the API token and the `X-Icaro-Project` header (plus optional `X-Icaro-Branch`, `X-Icaro-Agent`).

Agents should query pending handoffs before starting (`memory_query` with `kinds: ["handoff"], status: "pending"`), accept one with `memory_provenance` (`accept: true`), and leave a handoff when they stop. The session preamble (`memory.preamble` in the config) says so.

## HTTP fallback

For agents without MCP. Over the daemon socket no token is needed; over TCP send `Authorization: Bearer <token>`. `project` is a project id.

| Method and path | Body / query | Result |
|---|---|---|
| `POST /api/v1/memory` | `{project, kind, title, body?, source?, source_line?, ttl_seconds?, supersedes?, metadata?, branch?, worktree?, agent?, session_id?}` | the memory (201) |
| `GET /api/v1/memory` | `project, q?, kind? (comma list), branch?, agent?, since? (RFC 3339), source? (prefix), status?, limit?` | `{scope, count, results, timestamp}` |
| `GET /api/v1/memory/{id}` | `project` | provenance with edges |
| `GET /api/v1/memory/handoffs` | `project, branch?` | pending handoffs |
| `POST /api/v1/memory/{id}/accept` | `{project, agent?, session_id?}` | the accepted handoff |
| `POST /api/v1/memory/ingest` | `{path, project?, commits?}` | `{project, report}` |
| `POST /api/v1/memory/import` | `{project, format, data}` | `{imported, unchanged, edges}` |

Memories of another project answer 404.

## CLI fallback

```sh
icaro memory write --kind decision --title "Use SQLite" --body "single writer"   # --ttl 72h, --source, --agent
icaro memory query sqlite --kind decision --since 24h                           # --branch --source --status --limit --json
icaro memory show <id>
icaro memory handoff list            # --branch
icaro memory handoff accept <id>     # --agent
icaro memory ingest [PATH]           # --commits N (default 200, -1 none)
icaro memory import --format graphify graph.json
```

All commands take `--project <dir>` (default `.`) or `--project-id`.

## Deterministic ingest

`icaro memory ingest` walks the tree (skipping `.git`, `node_modules`, `vendor`, build output, hidden directories, binaries and files over 1 MiB) and records:

- each text file (path, size, language, first 2 KiB);
- symbols: Go via `go/parser` (functions, methods, types, exported consts/vars, with doc comments); JavaScript/TypeScript and Python via line-based patterns;
- imports, as `imports` edges from the file;
- ADRs: Markdown files in `adr/`, `adrs/` or `decisions/`, or named `NNNN-title.md`, with title and status;
- the most recent commits, with `touches` edges to their files.

Ids are derived from the content's identity, so re-ingesting unchanged files is a no-op, changed files are re-indexed, and files that disappeared are removed (only under the ingested subtree).

## Importers (prototype)

Adapters, not forks: Ícaro reads exports of other tools without depending on them.

- `graphify` — a knowledge graph in node-link JSON: `{"nodes":[{"id","label","type","source_file","source_location","description"}],"links":[{"source","target","relation"}]}` (`edges` accepted for `links`). Code node types map to `symbol`, files to `file`, decisions/concepts to `decision`.
- `ai-memory` — `{"memories":[...]}`, a JSON array, or JSON lines, each with `id, title, content, tags, namespace, tier, created_at, source, related`. A `decision`/`task`/`handoff` tag or kind selects the kind; `related` becomes edges.

Both formats are assumptions documented here; re-importing an unchanged export is a no-op.
