# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Ícaro runs many coding agents in parallel, one **session** per agent: a git worktree on its own branch, a hardened container with an egress allowlist, a live preview at `<name>.localhost`, and a scope in shared MCP memory. **Workflows** are DAGs of container (`image`) steps and declarative integration (`uses`) actions that orchestrate sessions.

The design lives in `blueprint/` and is binding. Before changing an area, read `blueprint/docs/01-architecture.md`, the engineering standards in `blueprint/docs/05-engineering.md`, and the phase file for that area in `blueprint/docs/phases/`. New decisions become an ADR under `blueprint/docs/adr/` plus a row in the decision index in `blueprint/docs/02-decisions-risks.md`.

## Commands

Requirements: Go (the version in `go.mod`), Docker for integration/e2e, Node 24 for `web/`. The module path is `icaro` (imports look like `icaro/internal/session`).

```sh
make build                     # bin/icaro (CGO_ENABLED=0); embeds web/dist if built
make web                       # npm ci + build the React UI into web/dist (needed before e2e / a real UI)
make test                      # go test -race -count=1 ./...  (unit, SQLite)
make test-integration          # adds -tags integration; needs a Docker daemon
make lint                      # golangci-lint (gosec, staticcheck, errcheck, unused, misspell)
make fmt                       # gofmt -s -w (CI fails on unformatted files)
make web-test                  # tsc --noEmit + vitest
make app-test                  # desktop app (separate module, cgo; GTK4/WebKitGTK6 headers on Linux)
```

Single tests:

```sh
go test -race -count=1 -run TestName ./internal/workflow
go test -race -count=1 -tags integration -run TestName ./internal/session
go test -run '^$' -fuzz '^FuzzParse$' -fuzztime 5m ./internal/workflow
cd web && npx vitest run src/path/to/file.test.ts
```

Generated files CI checks for drift — regenerate and commit them when you touch their inputs:

- `go generate ./...` → `schema/workflow.schema.json` from the workflow Go types (`internal/workflow/schema.go`); a drift test also compares it with `blueprint/docs/schemas/workflow.schema.json`.
- `go run ./hack/gendocs` → `docs/cli.md` from the Cobra tree (CI runs `-check`). Any CLI flag/command change needs this.
- `go run ./hack/licenses` (and the `-npm web` / `-dir desktop` variants in the `upgrade` Makefile target) → the `THIRD_PARTY_LICENSES.md` files, after dependency changes.
- `go run ./hack/linkcheck .` checks relative links in all Markdown/HTML.

Other suites: `go test -tags e2e ./hack/quickstart` and `./hack/e2e` (real Docker, after `make web build`), `cd web && npx playwright test` (UI e2e), `-tags load` for load tests in `internal/workflow/engine` and `internal/api`.

## Test conventions and coverage gate

- Build tags: `integration` (real Docker / daemon, optionally Postgres), `e2e`, `load`. Untagged tests must not need Docker.
- `ICARO_TEST_POSTGRES_DSN` makes the store tests also run against Postgres; every store change must pass on both SQLite and Postgres.
- CI enforces **95% statement coverage per `internal/*` package** in the unit run via `hack/covercheck`; exceptions with reasons are in `hack/covercheck/thresholds.txt` (ADR 0025). New code without tests will fail CI.
- Mock only the edges: clock, filesystem, process runner, external HTTP. Never mock the planner/executor to test them. Every bug fix gets a regression test first.
- Parsers of untrusted input have fuzz targets (`FuzzParse` in `internal/expr`, `internal/workflow`, `internal/integration`; `FuzzParseCompose` in `internal/preview`); commit crashers under `testdata/fuzz/`.
- `hack/docscheck` validates every workflow in `examples/` and every docs code block fenced as ```` ```yaml workflow ````; images there must be pinned by `@sha256:` digest.

## Architecture

**One daemon per user** (`icaro serve`, ADR 0001), holding a `flock` on `~/.icaro/run/icaro.lock` and serving HTTP on a Unix socket (TCP only with `--listen` + token). The CLI, web UI, desktop app and MCP server are **thin clients over one service layer**; business rules belong in the daemon, never in `internal/cli` or the UI. The CLI auto-starts the daemon when needed.

Request path: `cmd/icaro` → `internal/cli` (Cobra; each command file self-registers via `register()` in `init()`) → `internal/apiclient` → Unix socket → `internal/api` (HTTP handlers, SSE in `events.go`) → `internal/service` → domain packages.

`internal/app` is the composition root: it builds every component from config, owns their lifecycle (`Component.Run(ctx)`), and exposes one HTTP handler and one list of doctor checks. **Components never construct each other**; wiring is in `internal/app/*.go`.

Domain packages:

- `internal/session` — the core primitive (ADR 0002). Only owner of worktrees; manages the container, preview and memory facets, the `sessions` table, and **startup reconciliation** (non-terminal sessions are re-probed; unrecoverable ones become `orphaned`). A workflow `image` step runs as a session (ADR 0010).
- `internal/container` — images, mount policy, egress allowlist (`egress*.go`, `egressproxy/`), sandbox/gVisor runtime; `container/docker` is the Docker SDK adapter (covered only by integration tests).
- `internal/creds` — per-profile persistent agent homes under `~/.icaro/home/<profile>`: a read-mostly golden seed plus a per-session writable overlay (ADR 0005). The host's real `~/.claude` is never mounted.
- `internal/workflow` — spec/schema/validation (structured issues with JSON Pointer + nearest-match hints), DAG planner; `workflow/engine` executes runs (retries, timeouts, cancellation, pause/approval, log capture, re-attach after restart). `internal/expr` is the `${{ }}` / `when` expression language (ADR 0009); `internal/scheduler` handles persisted cron.
- `internal/integration` — declarative YAML manifests in git-backed packs (`builtin/` embedded; `local` pack is a git repo the UI commits to), webhook verification/dedup/redelivery, the in-daemon HTTP action executor for `uses:` steps, and the SSRF guard (`ssrf.go`). Manifests execute no code; integration credentials never enter a container.
- `internal/preview` — compose watcher, compose lint, Traefik file-provider routes (Traefik never gets the Docker socket).
- `internal/memory` + `internal/mcp` — FTS-backed project memory and the MCP server injected into agent sessions; scope is enforced server-side from session identity.
- `internal/store` — `database/sql` repositories over SQLite (`modernc.org/sqlite`, pure Go, one connection) or Postgres. Migrations are embedded per dialect in `internal/store/migrations/{sqlite,postgres}/NNNNN_name.sql`; **add a migration to both dialects** with the same number. Portable SQL only, JSON stored as text. Concurrent writes are made safe with compare-and-set updates (`... WHERE status IN (...)`) and transactions, not locks. `preview_instances.session_id`, `runs.session_id`, `step_runs.session_id` intentionally have no foreign key.
- `internal/desktop` — desktop app logic; it talks to the daemon over HTTP only and **must not import daemon internals** (CI checks its dependency list). The Wails shell is the separate Go module in `desktop/`.
- `web/` — React 19 + Vite + TypeScript UI, embedded into the binary from `web/dist` (`web/embed.go`); without a build the daemon serves `dist/README.txt`.
- `skills/` — the embedded `icaro-workflows` agent skill (`icaro skill install`).

## Rules that are easy to break

- **No mutexes in domain code**: goroutines own their state and communicate over channels. The store is the only exception, protected by SQL.
- **No new dependency** without meeting the dependency policy in `05-engineering.md`; a heavy HTTP framework, ORM, event bus, DI container, CGO library, or dependency on a specific agent needs an ADR. Prefer the standard library.
- **Security boundaries** (container baseline, egress allowlist, mount policy, secret handling/redaction, credential injection, `internal/api` auth, release signing in `internal/update` and `hack/sign`) are listed in `.github/CODEOWNERS` and `blueprint/docs/03-security.md`; don't weaken them.
- Destructive CLI commands require `--yes`.
- Logs are secret-redacted as they are written; keep bulky output in append-only log files, not SQL cells.
