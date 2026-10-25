# Ícaro

Ícaro runs **many coding agents in parallel** on your machine. Each agent gets its own **session**:

- a git worktree on its own branch;
- a hardened container that can reach only its provider's API;
- a live preview at a predictable URL;
- a scope in shared, MCP-accessible project memory.

**Workflows** orchestrate sessions: DAGs of container steps and declarative integration actions, triggered by hand, on a schedule, or by GitHub, GitLab, Slack, Jira, Linear, Sentry and Datadog events.

```text
icaro claude  ──►  worktree icaro/<name>  ──►  isolated container (egress allowlist, profile home)
                          │                              │
                          ├──► preview  http://<name>.localhost:8780
                          └──► memory   decisions, findings, handoffs (MCP)
```

## Quick start

```sh
curl -fsSL https://github.com/ggrocco/icaro/releases/latest/download/install.sh | sh   # or: brew install ggrocco/tap/icaro
icaro init && icaro setup          # ~/.icaro, master key in your keychain, profiles and images
icaro login claude                 # once per profile; never touches your own ~/.claude

cd ~/src/my-app
icaro claude                       # an agent in an isolated session on a new worktree
icaro session list

icaro workflow apply examples/hello.yaml
icaro run hello --set who=icaro --wait
icaro ui                           # the web UI, via a one-time loopback sign-in link
```

The [quickstart](docs/quickstart.md) walks through each step, and CI runs it against a real Docker engine.

## What you get

- **Sessions** ([docs](docs/sessions.md)):
  - one worktree and container per task;
  - per-profile persistent homes shared safely by concurrent sessions;
  - a mandatory egress allowlist and a strict mount policy;
  - a concurrency cap;
  - recovery after a restart.
- **Workflows** ([docs](docs/workflows.md)): a published JSON Schema and a validator whose issues point at the exact field, with nearest-match hints. Steps run in parallel as a DAG, with `${{ }}` data flow, `when` gating, retries, timeouts, cancellation, live logs over SSE, and re-attach after a daemon restart.
- **Integrations** ([docs](docs/integrations.md)): YAML manifests in git-backed packs. Webhooks are verified and deduplicated, every delivery is recorded and can be redelivered, actions run in the daemon with host-bound credentials and an SSRF guard, and cron triggers persist across restarts.
- **Previews** ([docs](docs/previews.md)): compose projects routed at `<slug>.localhost`, behind a compose lint that blocks privileged or host-escaping stacks.
- **Shared memory** ([docs](docs/memory.md)): decisions, findings, tasks and handoffs per project, plus indexed files, symbols, ADRs and commits, served to agents over MCP.
- **Agents as users** ([docs](docs/agents.md)): MCP authoring tools and the `icaro-workflows` skill (`icaro skill install`).
- **Web UI** ([docs](docs/ui.md)): form and YAML editors, run graphs with live status, deliveries, and a manifest editor that commits to your local pack.
- **Desktop app** ([docs](docs/desktop.md)): a menu bar app with runs, previews, usage and quick actions, notifications on failures and outages, and launch at login. It has not yet been run on a clean Mac.
- **Operations:** one daemon per user on a Unix socket, `icaro doctor --fix`, signed releases with `icaro update --rollback`, `icaro service install`, and `icaro usage`.

Read [Security](docs/security.md) for what the isolation does and does not cover.

## Documentation

- User docs: [`docs/`](docs/README.md), including the [CLI reference](docs/cli.md).
- Design, decisions and roadmap: [`blueprint/`](blueprint/README.md). The current state of each phase is in the [roadmap status](blueprint/docs/04-roadmap.md#status).

## Development

Requirements: Go 1.26+, Docker, and Node 24 for the web UI.

```sh
make web build                 # embed the UI, build bin/icaro
make test                      # unit tests (-race, SQLite)
make test-integration          # needs Docker; set ICARO_TEST_POSTGRES_DSN to add Postgres
make lint                      # golangci-lint
make app-test app              # the desktop app (cgo; see CONTRIBUTING.md)
go generate ./...              # regenerate schema/workflow.schema.json
go run ./hack/gendocs          # regenerate docs/cli.md
go test -tags e2e ./hack/quickstart   # run the quickstart against real Docker
cd web && npx playwright test  # UI end-to-end tests (after make web build)
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md). Licensed under the [MIT license](LICENSE).
