# Agents and MCP

Connect Claude Code, Codex, or another MCP client to Ícaro to create workflows in natural language and share context across sessions.

- **MCP (Model Context Protocol)** is the connection: it exposes tools for discovering, validating, running, and debugging workflows, plus reading and writing project memory.
- **A skill** is a set of instructions for the agent. Ícaro bundles [`icaro-workflows`](../skills/icaro-workflows/SKILL.md), which teaches the workflow-authoring loop. The skill uses the MCP tools; install both to follow the tutorial.

The host daemon exposes workflow authoring and memory. Agents inside isolated Ícaro sessions get a separate, memory-only MCP endpoint automatically; they cannot reach the daemon's authoring API.

## Connecting an agent on your machine

```sh
icaro skill install                 # Claude Code: skill + MCP entry in ~/.claude.json
icaro skill install --target codex  # Codex: skill + [mcp_servers.icaro] in ~/.codex/config.toml
```

The command writes the skill and an MCP server entry that runs `icaro mcp` using the current executable's absolute path:

| Client | Skill location | MCP configuration |
| --- | --- | --- |
| Claude Code | `~/.claude/skills/icaro-workflows/SKILL.md` | `~/.claude.json`, under `mcpServers.icaro` |
| Codex | `~/.codex/skills/icaro-workflows/SKILL.md` | `~/.codex/config.toml`, under `[mcp_servers.icaro]` |

`icaro mcp` is a stdio bridge to the daemon's `/mcp` endpoint, which accepts MCP streamable HTTP and uses the same authentication as the API. Its memory scope is the project of the current git repository, or of `--project PATH`. `--no-mcp` writes only the skill; `--home-dir` installs into another home directory. `icaro skill list` lists the bundled skills.

Restart your agent client after installation. If you move the `icaro` executable, update the MCP command path in the client configuration. For Codex, the installer leaves an existing `[mcp_servers.icaro]` entry in place, so check its path if reconnecting fails.

Agents running **inside a session** need none of this. Each session gets its own MCP socket, mounted at `/icaro/mcp.sock`, and the session's agent config already runs `icaro mcp --socket /icaro/mcp.sock`. Ícaro sets the scope from the session's identity, and nothing the agent sends can change it ([ADR 0019](../blueprint/docs/adr/0019-memory-mcp-transport.md)).

## Tutorial: your first workflow with an agent

### 1. Prepare and check the connection

Complete [installation and initialization](quickstart.md#1-install), including `icaro init` and `icaro setup`, and make sure Docker is running. Install the skill for your host client using one of the commands above. This example needs no integration credentials or agent-provider login inside Ícaro; your host agent still uses its own account.

Start your agent client from an existing project repository:

```sh
cd ~/src/my-app
icaro skill list
icaro serve --background
# Start Claude Code or Codex here.
```

If the daemon is already running, skip `icaro serve --background`. Ask your agent:

> Check that the icaro MCP server is connected. Call get_workflow_schema and list_integrations, and tell me which workflow step types and integrations are available.

A successful connection returns the workflow schema and the installed integrations. Client interfaces may prefix tool names with the server name `icaro`.

### 2. Ask for a small workflow

> Use the icaro-workflows skill to create a manual workflow named mcp-hello. Take a `who` input, greet that person in an Alpine container, and publish the greeting as an output. Fetch the schema first, validate the workflow, run it with who=icaro, and show me the result and logs. If mcp-hello already exists, inspect it before updating it.

The agent should discover the schema and integrations, check secret names, validate until clean, create the workflow, run it, and inspect the run. This example needs no secrets. A valid document is included as [`examples/mcp-hello.yaml`](../examples/mcp-hello.yaml):

```yaml workflow
version: 1
name: mcp-hello
description: A first workflow authored with the icaro-workflows skill
triggers:
  - type: manual
steps:
  - id: greet
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    input:
      who: ${{ trigger.who }}
    command: |
      printf 'hello %s\n' "$ICARO_INPUT_WHO"
    outputs:
      greeting: stdout:last-line
```

`who` reaches the command through an environment variable rather than being inserted into shell source. The last line of stdout becomes the `greeting` output.

For clients that let you inspect tool arguments, the important calls are:

| Tool | Arguments / expected result |
| --- | --- |
| `get_workflow_schema`, `list_integrations`, `list_secrets` | `{}` for each; discover the contract and available capabilities. |
| `validate_workflow` | `{"yaml":"<complete YAML document>"}`; expect `valid: true` and no issues. |
| `create_workflow` | `{"yaml":"<same validated document>"}`; stores `mcp-hello`. |
| `run_workflow` | `{"name":"mcp-hello","trigger":{"who":"icaro"}}`; returns `run_id`. |
| `get_run` | `{"id":"<returned run_id>"}`; inspect again until the run finishes. |

Use `update_workflow` for an existing workflow; `create_workflow` refuses to overwrite it. A manual run must supply every referenced `trigger.*` field.

### 3. Inspect the result

Expect a `succeeded` run, a `greet` step with exit code `0`, output `greeting: hello icaro`, and a log line `hello icaro`.

```sh
icaro runs list --workflow mcp-hello
icaro ui
```

In the UI, open **Workflows → mcp-hello** and select its run to see the graph, outputs, and logs. To follow a run in the terminal, use `icaro runs logs RUN_ID --follow`, replacing `RUN_ID` with the returned ID.

You can also try the same example directly from a checkout of this repository:

```sh
icaro workflow validate examples/mcp-hello.yaml
icaro workflow apply examples/mcp-hello.yaml
icaro run mcp-hello --set who=icaro --wait
```

### 4. Share context with the next session

MCP also exposes memory scoped to your project. Try these prompts in order:

> Use memory_write to record a finding titled "First workflow verified" with the body "mcp-hello succeeded with who=icaro; greeting was hello icaro."

> Use memory_query to find "First workflow verified", then show its source and scope.

> Before ending this session, write a handoff describing what we verified and suggesting one next step. At the start of the next session, query pending handoffs and inspect the relevant one before accepting it.

Start the host client in the repository so Ícaro can derive the memory project. Worktrees of the same repository share memory; different repositories have separate scopes. Workflow authoring operates on the daemon's workflow catalog, not a separate catalog per memory project. See [Shared memory](memory.md) for provenance, ingestion, and handoff details.

## Other MCP clients and explicit project scope

For a client that accepts a JSON stdio-server configuration, merge this entry into its MCP settings. Replace `/absolute/path/to/my-app` with your repository path, and use the absolute path to `icaro` if the client cannot find it on `PATH`:

```json
{
  "mcpServers": {
    "icaro": {
      "command": "icaro",
      "args": ["mcp", "--project", "/absolute/path/to/my-app"]
    }
  }
}
```

For Codex, the equivalent table in `~/.codex/config.toml` is:

```toml
[mcp_servers.icaro]
command = "icaro"
args = ["mcp", "--project", "/absolute/path/to/my-app"]
```

An explicit project path is useful for clients launched outside a repository. It fixes this connection's memory scope to that repository. MCP configuration connects the tools; clients that support skills can also load the bundled [skill instructions](../skills/icaro-workflows/SKILL.md).

The client launches `icaro mcp` itself. Running it in a terminal waits for JSON-RPC on stdin; it does not open an interactive chat.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| No `icaro` tools appear | Restart the agent client, check its MCP status, and confirm that the configured `command` points to the installed executable. |
| Tools appear, but the skill is unavailable | Confirm the skill file is in the location shown above and restart the client. `--no-mcp` installs only the skill, so use it only when a connection is already configured. |
| The bridge cannot reach the daemon | Run `icaro doctor`; check that the client and daemon use the same `ICARO_HOME` and configuration. For a remote daemon, configure `daemon.url` and `daemon.token` as described in the [CLI reference](cli.md). |
| Memory reports a missing project scope | Start the client in a git repository, or set `--project` in the MCP server arguments. Do not pass `project_id` to tools. |
| Only memory tools are available | This is expected inside an isolated session. Use a host MCP connection for workflow authoring. |
| Validation fails | Read each issue's `path`, `message`, and `hint`; correct the YAML and validate again before saving. |
| A run fails or stays queued | Ask the agent for `get_run`, check Docker with `icaro doctor`, and inspect the reported step error and logs. |
| A secret or approval is needed | Create the credential yourself with `icaro secret set NAME` or `icaro credential add INTEGRATION`. Decide approvals in the UI or CLI. Tools expose secret names, never values, and cannot approve a step. |

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
