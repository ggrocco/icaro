# Phase 11 — Hardening and GA

**Objective:** reliability, performance, and security before GA.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Load tests for workflows and SSE. *(`internal/workflow/engine/load_test.go`, `internal/api/sse_load_test.go`; `-tags load` for full size.)*
- [x] Chaos tests: Docker restarts, daemon crashes, DB lock. *(`internal/chaos/`: daemon SIGKILL with real Docker, a Docker outage via a fault-injecting client, SQLite locked by a second process. A real Docker daemon restart is not exercised.)*
- [x] Dependency/license audit. *(`hack/licenses`, `THIRD_PARTY_LICENSES.md`, `web/THIRD_PARTY_LICENSES.md`, govulncheck in CI.)*
- [x] Performance profiling and memory/CPU budget. *(`icaro serve --pprof` on the Unix socket; budget in [ADR 0024](../adr/0024-hardening-budget.md).)*
- [ ] Compatibility matrix and public beta. *(Matrix below; the public beta is a release decision, not a code change.)*

## Acceptance criteria

- [x] SLOs met on the reference hardware. *(See [ADR 0024](../adr/0024-hardening-budget.md).)*
- [x] No deadlock/race under `-race` where applicable. *(`go test -race -tags integration ./...` is clean; repeated `-count=3` runs found one real ordering bug, fixed.)*
- [x] Doctor resolves every scenario in the [doctor checks](../03-security.md#icaro-doctor). *(`internal/cli/doctor_coverage_test.go` maps every row to a registered check. Rows whose remediation is a user action — DNS, a missing webhook secret, a dangerous compose file — report the exact command or field instead of changing anything.)*
- [ ] The release candidate passes smoke tests on macOS/Linux. *(Linux: the E2E and chaos suites. macOS: not runnable here.)*

## Compatibility matrix

| Component | Tested | Notes |
|---|---|---|
| Linux amd64 | ✅ unit, integration, chaos, load, E2E | Reference machine for [ADR 0024](../adr/0024-hardening-budget.md) |
| Linux arm64 | build only (CI cross-compile) | |
| macOS arm64 / amd64 | unit tests in CI; build | Docker Desktop paths (VirtioFS, polling watch, Keychain) are written but not run end to end |
| Docker Engine | 29.9 (API 1.56) | Older engines untested; the client negotiates the API version |
| Docker Desktop (macOS) | untested | Spikes S1/S2 need a Mac |
| Docker Compose | v5.6 plugin | Previews call `docker compose` |
| Go | 1.26.0 (`go.mod`); stable patch for govulncheck | |
| Node.js | 22 | Web UI build and Playwright E2E |
| Chromium (Playwright) | bundled with the runner image | E2E only |
| SQLite | `modernc.org/sqlite` 1.60 (pure Go) | Default store |
| PostgreSQL | 16 | Store tests run against both dialects |
| Wails (tray app) | v3.0.0-beta.34 | [ADR 0027](../adr/0027-desktop-app.md); CI builds and tests the app on macOS and Linux; not yet run on a Mac |

## Measured budget

See the table in [ADR 0024](../adr/0024-hardening-budget.md). To re-measure: `go test -tags load -run Load -v ./internal/workflow/engine ./internal/api`, and for a running daemon, `icaro serve --pprof` then `curl --unix-socket ~/.icaro/run/icaro.sock http://icaro/debug/pprof/heap?debug=1`.
