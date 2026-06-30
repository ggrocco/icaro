# Decisions, cuts & risks

| Topic | Decision | Rationale | Risk / Mitigation |
|---|---|---|---|
| Core primitive | The **session** unifies worktree+container+preview+memory; the product is parallel isolated agents (ADR 0002) | One product, not four competing tools; clear install reason | Spine must work end-to-end before breadth features; sequence the roadmap around it |
| Credentials | **Brokered, not raw-mounted** (ADR 0003) | Bounds blast radius of an injected agent — the #1 threat | Each provider needs a broker adapter; long-lived-key providers documented as residual risk |
| Memory interface | Exposed to agents over **MCP** (ADR 0004) | Target CLIs already speak MCP; avoids "a DB nobody writes to" | Non-MCP agents get an HTTP/CLI fallback; scope enforced server-side |
| Workflow scope | Orchestrates **sessions**, not general CI | Avoids competing with `act`/Dagger; smaller surface | Data flow (`outputs`/`env`/interpolation) is mandatory in the contract, not optional |
| Stronger isolation | Evaluate per-container microVMs (Apple Containerization / gVisor / Firecracker) in the Phase 0/1 spike | Docker non-root is a weak boundary for semi-trusted LLM output | If microVMs slip, untrusted profiles still get a mandatory egress allowlist |
| macOS UI | Wails v3 alpha only behind an interface; assess stability before GA | Tray and Go/React integrated | Keep `internal/platform` isolated and test Wails v2 if v3 does not stabilize |
| DB | Local SQLite (`modernc.org/sqlite`, pure Go) | local-first, offline, no CGO | Keep storage behind a small store interface so a driver swap stays cheap |
| Scheduler | a small in-house engine, based on `time` + a well-established external cron parser | control over persistence and recovery | the scheduler cannot be the single source; persist next-run and leases |
| Workflow UI | a React Flow canvas only in the web module | complex drag/drop is not worth implementing from scratch | the schema is the source of truth; the UI is only an editor |
| Preview proxy | Traefik | Docker provider, labels and ACME DNS | requires a clear policy for ports, network and DNS |
| Container sandbox | local Docker, non-root and without socket | a good initial balance | not a strong boundary sandbox; document the threat and offer a future VM mode |
| Memory | FTS + deterministic graph first | reduces cost, latency and dependencies | vectors come in after a quality metric |
| Updates | Brew for macOS; a signed/checksummed release for the script | conventional distribution | updates only download signed/checksummed artifacts |

## Mandatory MVP cuts

1. No unified usage accounting across providers; only process telemetry when available.
2. No remote execution over TCP beyond loopback.
3. No automatic DNS provisioning; only integration with a configured provider.
4. No "Bash execution on the host" by default.
5. No advanced visual editor before the JSON Schema and executor are stable.
6. Previews cover **compose-based projects with a declared port** only; host-run dev servers (Vite/Next on the host) are out of MVP scope.
7. The native macOS app is **deferred until the session spine works end-to-end**; the web UI served by the daemon is the MVP surface (the native app is polish, not the wedge).
8. Workflows orchestrate sessions only; no general-purpose CI step library beyond the five MVP step types.

## Risks needing a spike before extensive build

- Compatibility and maturity of Wails v3 for the tray/menu bar.
- The credential behavior of each agent inside the container.
- Local DNS for `.test` on macOS and coexistence with VPNs.
- Heterogeneous Compose: projects without a clearly declared HTTP port.
- Status/usage semantics of the CLIs: each vendor exposes different data.
