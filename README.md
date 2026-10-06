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
bin/icaro init                               # data dir, master key, schema
export ICARO_SERVER__TOKEN=$(bin/icaro token create --name dev --scope admin)
bin/icaro serve &                            # API on 127.0.0.1:8787 + runner

bin/icaro workflow validate examples/hello.yaml
bin/icaro workflow apply examples/hello.yaml
bin/icaro run hello --input '{"who":"icaro"}' --wait
bin/icaro runs logs <run-id> 0
```

Configuration lives in `icaro.yaml` (see `bin/icaro config show`) and can be
overridden with `ICARO_*` environment variables, nested keys joined by `__`
(`ICARO_DATABASE__DRIVER=postgres`).

## The step contract

| Channel | Mechanism |
|---|---|
| Inputs | `ICARO_INPUT_<NAME>` env vars + `/icaro/input.json` (`/icaro/context.json` has inputs, earlier step outputs and run metadata) |
| Outputs | Write a JSON object to `/icaro/output.json` → `{{ steps.<name>.outputs.<key> }}` |
| Files | `/workspace` is shared by all steps of a run |
| Status | Exit code `0` = success |
| Credentials | `connection: <name>` injects `ICARO_CONN_<FIELD>`; values are scrubbed from logs |

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
