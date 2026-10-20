# Agents and MCP

The daemon hosts one MCP server. Agents use it to author and run workflows and to read and write shared memory, with no integration specific to Ícaro.

## Connecting an agent on your machine

```sh
icaro skill install                 # Claude Code: skill + MCP entry in ~/.claude.json
icaro skill install --target codex  # Codex: skill + [mcp_servers.icaro] in ~/.codex/config.toml
```

The command writes the [`icaro-workflows` skill](../skills/icaro-workflows/SKILL.md) and an MCP server entry that runs `icaro mcp`. `icaro mcp` is a stdio bridge to the daemon's `/mcp` endpoint, which accepts MCP streamable HTTP and uses the same authentication as the API. Its memory scope is the project of the current git repository, or of `--project PATH`. `--no-mcp` writes only the skill; `--home-dir` installs into another home directory.

Agents running **inside a session** need none of this. Each session gets its own MCP socket, mounted at `/icaro/mcp.sock`, and the session's agent config already runs `icaro mcp --socket /icaro/mcp.sock`. Ícaro sets the scope from the session's identity, and nothing the agent sends can change it ([ADR 0019](../blueprint/docs/adr/0019-memory-mcp-transport.md)).

## Authoring tools

| Tool | What it does |
|---|---|
| `get_workflow_schema` | The workflow JSON Schema. Agents fetch it before drafting and never write fields from memory. |
| `list_integrations` / `get_integration` | Installed integrations, their events (with `trigger.*` fields) and actions (typed inputs, outputs, hosts) |
| `list_secrets` | Secret names and host bindings, **never values** |
| `validate_workflow` | Structured issues: JSON Pointer `path`, `message`, `hint` |
| `create_workflow` / `update_workflow` | Store a workflow that validates cleanly (create refuses an existing name; update refuses a missing one) |
| `get_workflow` / `list_workflows` | Current YAML, versions, the trigger fields a manual run needs |
| `run_workflow` | Start a manual run with a `trigger` payload |
| `get_run` | Status, per-step outputs and errors, and a size-capped, redacted log tail (4 KiB by default, 32 KiB at most) |

The server also exposes two resources: `icaro://schemas/workflow` and `icaro://schemas/actions`, which has one input schema per installed action.

There are **no destructive tools**: agents author and run, and deleting is left to people. A `read`-scope caller cannot create, update or run anything. The skill teaches the loop:

1. Fetch the schema and integrations.
2. List the secrets and ask the human for any that are missing.
3. Validate until clean.
4. Create the workflow and run it with a small input.
5. Debug with `get_run`.

## Memory tools

`memory_write` (decision, task, finding, handoff), `memory_query` (full-text search with filters) and `memory_provenance` work as described in [Shared memory](memory.md). Every result carries its source, timestamp and scope.

## Without MCP

Everything above is also available over the HTTP API (`/api/v1/...`) and the CLI: see [CLI reference](cli.md) and the memory [HTTP/CLI fallback](memory.md#http-fallback).
