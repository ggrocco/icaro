# Icaro

Icaro runs workflows whose steps are containers. Every step gets the same
contract — inputs in, `/icaro/output.json` out, a shared `/workspace`, exit
code as status — under a hardened sandbox, triggered by API, webhooks or
schedules, and authored by humans or AI agents against a published JSON
Schema.

Design and roadmap: [`blueprint/`](blueprint/README.md). The code below was built
from an earlier design and is migrating toward the blueprint; see
[existing code](blueprint/docs/04-roadmap.md#existing-code).

> Status: Phase 1 (engine core). Workflows, runs, connections, tokens,
> CLI and REST API work; the runner executes `run` steps on Docker.
> Triggers, the `http` step, MCP server, UI and tray are next.

## Quick start

Requirements: Go 1.26+, a Docker daemon reachable by the server.

```sh
make build                                   # bin/icaro
bin/icaro init                               # ~/.icaro, master key, schema
bin/icaro serve --background                 # one daemon per user, Unix socket

bin/icaro workflow validate examples/dag.yaml
bin/icaro workflow apply examples/hello.yaml
bin/icaro run hello --set who=icaro --wait   # streams logs, exits 1 unless it succeeds
bin/icaro runs logs <run-id> greet
```

Configuration lives in `icaro.yaml` (see `bin/icaro config show`) and can be
overridden with `ICARO_*` environment variables, nested keys joined by `__`
(`ICARO_DATABASE__DRIVER=postgres`).

## The step contract

| Channel | Mechanism |
|---|---|
| Inputs | The step's resolved `input` in `/icaro/input.json`, scalars also as `ICARO_INPUT_<NAME>` |
| Outputs | Declared per step: `json:<path>` into `/icaro/output.json` (a JSON object, ≤ 1 MiB), `stdout`, `stdout:last-line`, or `file:<path>`; read later as `${{ steps.<id>.outputs.<key> }}` |
| Files | `/workspace` — the run session's workspace (or a `repository` worktree), shared by the run's steps |
| Status | Exit code `0` = success; `needs`, `when`, `retry` and `timeout` decide the rest ([ADR 0013](blueprint/docs/adr/0013-workflow-execution-semantics.md)) |
| Secrets | `${{ secrets.<name> }}` in `env`, resolved by the daemon; every secret value is redacted from logs and captured outputs |
| Logs | stdout/stderr, indexed by offset; `GET /api/v1/runs/{id}/events` streams status and log lines (SSE) |

Every container runs with all capabilities dropped, `no-new-privileges`, a
seccomp profile without `ptrace`/`bpf`/`mount`/namespace/module syscalls, a
read-only root filesystem (tmpfs `/tmp` and `/home`), pid/memory/cpu limits,
and only the run's volumes mounted. There is no way to weaken this from a
workflow file.

## Verifying the sandbox contract on a Docker host

```sh
go run ./hack/spike-io
```

## Development

```sh
make test                 # unit tests (SQLite)
ICARO_TEST_POSTGRES_DSN=postgres://icaro:icaro@localhost:5432/icaro?sslmode=disable make test
make test-integration     # needs Docker
make lint                 # golangci-lint
go generate ./...         # regenerate schema/workflow.schema.json after editing internal/workflow/spec.go
```
