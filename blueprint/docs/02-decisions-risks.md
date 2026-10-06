# Decisions, cuts & risks

## Decision index

| Topic | Decision | Rationale | Risk / Mitigation |
|---|---|---|---|
| Source of truth | `blueprint/` supersedes the earlier workflow-engine design in `docs/` ([ADR 0008](adr/0008-blueprint-supersedes-engine-design.md)) | One design to build against | The existing code follows the old design; the [existing-code table](04-roadmap.md#existing-code) schedules each change |
| Process model | One daemon per user; CLI and Wails are clients ([ADR 0001](adr/0001-single-daemon.md)) | No race between the app and `icaro serve`; one place for logs/events | Every feature must be reachable through the daemon contract and testable without Wails |
| Core primitive | The **session** unifies worktree+container+preview+memory; the product is parallel isolated agents ([ADR 0002](adr/0002-session-spine.md)) | One product, not four competing tools; clear install reason | Spine must work end-to-end before breadth features; the roadmap is sequenced around it |
| Session state | `sessions` is a first-class table, and the daemon runs a startup reconciliation pass ([ADR 0006](adr/0006-session-persistence-recovery.md)) | Crash, restart, and upgrade need a data model to recover from | Sessions it cannot re-attach to are marked `orphaned` and surfaced by `icaro doctor` |
| Credentials | A **persistent per-profile home** (golden seed + per-session overlay) bounded by a **mandatory egress allowlist** ([ADR 0005](adr/0005-persistent-home-egress.md), superseding the broker default of [ADR 0003](adr/0003-credential-broker.md)) | Agent CLIs authenticate from their own home; none supports a scoped-token broker | The secret is inside the container; see [residual risks](#residual-risks). A credential-injecting proxy is future hardening for untrusted profiles |
| Container sandbox | Local Docker, non-root, no Docker socket, mandatory egress allowlist; per-container microVMs (Apple Containerization / gVisor / Firecracker) evaluated in spike S5 | A good initial balance; Docker non-root is a weak boundary for semi-trusted LLM output | If microVMs slip, the mandatory egress allowlist remains the boundary; document the threat |
| Memory interface | Exposed to agents over **MCP** ([ADR 0004](adr/0004-memory-mcp.md)) | Target CLIs already speak MCP; avoids "a DB nobody writes to" | Non-MCP agents get an HTTP/CLI fallback; scope enforced server-side |
| Agent interface | Agents author and run workflows through MCP tools + a shipped skill ([architecture §10](01-architecture.md#10-agent-interface)) | Agents are first-class users; schema + validator make authoring converge | No destructive tools in v1; in-container access is deferred |
| Memory retrieval | FTS + deterministic graph first | Reduces cost, latency and dependencies | Vectors come in after a quality metric |
| Workflow scope | Orchestrates **sessions**, not general CI; a step is `image` or `uses` | Avoids competing with `act`/Dagger; smaller surface | Data flow (`outputs`/`env`/interpolation) is mandatory in the contract, not optional |
| Integrations | **Declarative manifests in git-backed packs** ([ADR 0007](adr/0007-integration-manifests.md)) | Adding an integration is a YAML file, not a release; first- and third-party use the same seam | Daemon executes manifest-defined HTTP, guarded by host-bound creds, an SSRF guard, and a permission summary; `image` steps are the escape hatch when a template can't express an API |
| Previews | The worktree is bind-mounted (not copied) into separate agent and dev-server containers; hot reload is the app's job; polling watch is mandatory on macOS ([architecture §7](01-architecture.md#7-previews)) | Docker Desktop keeps data coherent across VirtioFS but drops inotify events | Polling costs CPU per active session, which feeds the concurrency cap |
| Preview proxy | Traefik | Docker provider, labels and ACME DNS | Requires a clear policy for ports, network and DNS |
| Usage / cost | codeburn-style local log parsing (reads `~/.claude/` etc.) for Phase 9 | Works without provider APIs | It is a cost tracker, not a compute limiter — it does not bound RAM/CPU/containers |
| macOS UI | Wails v3 alpha only behind an interface; assess stability before GA | Tray and Go/React integrated | Keep `internal/platform` isolated and test Wails v2 if v3 does not stabilize |
| DB | Local SQLite (`modernc.org/sqlite`, pure Go) behind a `database/sql` store; Postgres as the configured alternative | Local-first, offline, no CGO | Portable SQL only and both backends in CI, or the swap rots |
| Auth | Single user: socket/directory permissions locally; hashed, scoped API tokens for TCP; pasted-token cookie sessions for the web UI | Simple, no account system in the MVP | Multi-user/OIDC later behind the same session table |
| Scheduler | A small in-house engine based on `time` + a well-established external cron parser | Control over persistence and recovery | The scheduler cannot be the single source; persist next-run and leases |
| Workflow UI | A React Flow canvas, only in the web module | Complex drag/drop is not worth implementing from scratch | The schema is the source of truth; the UI is only an editor |
| Updates | Brew for macOS; a signed/checksummed release for the install script | Conventional distribution | Updates only download signed/checksummed artifacts |

## Mandatory MVP cuts

1. No unified usage accounting across providers; only process telemetry when available.
2. No remote execution over TCP beyond loopback.
3. No automatic DNS provisioning; only integration with a configured provider.
4. No "Bash execution on the host" by default.
5. No advanced visual editor before the JSON Schema and executor are stable.
6. Previews cover **compose-based projects with a declared port** only; host-run dev servers (Vite/Next on the host) are out of MVP scope.
7. The native macOS app is **deferred until the session spine works end-to-end**; the web UI served by the daemon is the MVP surface.
8. Workflows orchestrate sessions only: a step is a container image or a declarative integration action, and conditions are `when` expressions — not a general-purpose CI step library.
9. Integration manifests v1 exclude OAuth flows (token/PAT first), pagination, polling triggers, and scripting hooks; inbound webhooks assume a reachable daemon, and laptop users fall back to manual/cron.

## Open items

These need a decision before the phase that depends on them starts.

| # | Item | Proposed default / status | Blocks |
|---|---|---|---|
| O1 | Compute budget for N parallel sessions | A **configurable concurrency cap** (active sessions ≤ K; the rest are persisted as `queued`) plus per-container `--memory`/`--cpus` limits; default K derived from detected RAM. Each session is an agent container **plus** a preview compose stack, and Docker Desktop's fixed VM memory ceiling is hit fast on a laptop. | Spine milestone |
| O2 | How an `image` step maps onto a session | Unresolved. The step model says "a step is an image"; Phase 2 says "the unit of work a step drives is a session". The answer also decides which profile (egress allowlist, isolation) a non-agent image step runs under. | Phase 2 executor |
| O3 | `when` expression grammar | Unwritten; the schema leaves it to semantic validation. Trigger `filter` expressions need the same answer. Constraints any grammar must meet: expressions are parsed at validation time, not first run; a missing key is an error; functions come from an allow-list (no env, file, or exec access); a step may reference only its `needs` ancestors. | Phase 2 executor |
| O4 | Explicit "shared home" profile vs. agent credential directories | Unresolved. The mount policy allows an explicit shared-home profile, but ADR 0005 forbids mounting the host's real `~/.claude`. Does the shared-home profile exclude `~/.claude`, `~/.config/gh`, `~/.aws` and similar, or is a raw credential mount allowed behind a setup-time warning? | Phase 1 mount policy |
| O5 | How the browser reaches the web UI | Unresolved. MVP cut 7 makes the daemon-served web UI the MVP surface, but the daemon listens only on a Unix socket unless started with `--listen` and a token (architecture §3), and a browser cannot reach a Unix socket. Options include a loopback TCP listener enabled by default for the UI, or requiring `--listen`. | Phase 4 |

## Feasibility gate

Per the project rule that doubts are resolved up front, every spike has a pass criterion and a fallback. **Week-0** spikes gate the Phase 0 build. **Phase-gated** spikes must pass before the named phase starts.

| Spike | Gate | Question to answer | Pass criterion | Fallback if it fails |
|---|---|---|---|---|
| **S1 — macOS file propagation** | Week 0 | With agent and preview in separate containers on the same host worktree, does the preview dev server reflect an agent's edit on Docker Desktop? | An agent-container edit produces a visible preview change in < ~3 s with polling enabled; VirtioFS keeps data coherent. | Document host-run dev servers as the macOS path; native inotify is not promised. |
| **S2 — per-session home overlay** | Week 0 | Can a golden home (creds) + per-session writable overlay be mounted into the agent container on Docker Desktop and Linux? | Two concurrent sessions share the seeded login yet write to isolated layers with no `~/.claude` corruption. | Per-session full-copy home (more disk, slower start) or serialize sessions sharing a profile. |
| **S3 — MCP transport into the container** | Week 0 | "MCP over the session Unix socket" ([ADR 0004](adr/0004-memory-mcp.md)) is not a standard MCP transport, because agent CLIs expect stdio or HTTP. How is the endpoint injected into the container and reached from inside it? | A target CLI (Claude Code/Codex) reads/writes memory via an injected MCP config that bridges to the daemon, with scope derived from session identity. | A small stdio bridge binary baked into the agent image that proxies to the bind-mounted session socket. Candidate design: an in-image shim forwards a loopback port to the socket, so the CLI's stock HTTP MCP config works unmodified; the daemon side injects session identity and auth, so no token enters the container and it works with no network. |
| **S4 — credential reality per agent** | Week 0 | Does each target agent CLI actually authenticate from the persistent home alone, with **no** usable provider secret leaking into a place we don't control? | For each supported CLI, the only creds present are in the seeded home, and the mandatory egress allowlist confines their use. | Per-agent adapter; downgrade unsupported agents to a documented manual-login profile. |
| **S5 — microVM isolation** (optional, time-boxed) | Week 0 | Is per-container microVM isolation (Apple Containerization on Apple Silicon, gVisor/Firecracker on Linux) viable for untrusted profiles? | A spike-quality prototype runs an agent under the stronger boundary. | The mandatory egress allowlist remains the boundary for untrusted profiles. |
| **S6 — heterogeneous Compose** | Phase 5 | How do real projects without a clearly declared HTTP port behave under the port-discovery rule? | The `icaro.preview.port` label + unambiguous-`80` fallback routes the sample set without guessing. *(proposed)* | Require the label; document unsupported layouts. *(proposed)* |
| **S7 — local DNS for `.test`** | Phase 5 (`.test` deliverable) | Does local `.test` resolution work on macOS and coexist with VPNs? | Resolution works with and without a common VPN client active. *(proposed)* | `http://localhost:<port>` as the documented default. *(proposed)* |
| **S8 — manifest expressiveness** | Phase 3 (before freezing manifest schema v1) | Can declarative HTTP action templates cover the real Slack/Jira/Datadog APIs? | All seven bundled manifests work against the live APIs. *(proposed)* | Extend the template language, or route the gap through an `image` step. *(proposed)* |
| **S9 — Wails v3 maturity** | Phase 7 | Is Wails v3 stable enough for the tray/menu bar? | Tray, menu bar, and launch-at-login work on a clean macOS install. *(proposed)* | Wails v2 behind the same `internal/platform` interface. *(proposed)* |
| **S10 — CLI usage semantics** | Phase 9 | What status/usage data does each vendor's CLI actually expose? | Each supported source is parsed with its source and confidence recorded. *(proposed)* | Show only the sources that can be parsed reliably; never infer cost. *(proposed)* |

## Deferred designs

Each design below is compatible with the product but blocked by an MVP cut or ADR. It returns only through a new ADR that lifts or amends the blocker ([ADR 0008](adr/0008-blueprint-supersedes-engine-design.md)).

| Design | Blocked by | Notes worth keeping |
|---|---|---|
| Workflow library: shared scripts and step presets, mounted read-only at `/icaro/lib` | Cut 8 (adds `script:`/`preset:` step forms) | Materialize content-addressed and pin the snapshot per run, so every step of a run sees the same version |
| `call_workflow` sub-workflow step and in-container MCP access (`none`/`read`/`invoke`/`author`) | Cut 8 | Workflows authored by an agent inside a step land as **inert drafts** until a human approves them, because agent steps process untrusted payloads. Recursion needs brakes: run lineage (`root_run_id`/`parent_run_id`, already in the code's schema), a depth cap, a per-root budget of descendant runs, and cycle rejection |
| `agent` step type with a harness catalog and declared params | Cut 8 (an agent is an `image` step) | Pin harness images by digest; declared params make each harness's knobs validatable |
| `wait` / `approval` steps | Cut 8 | — |
| GitHub App mode: App Manifest setup flow, one endpoint for all installations, installation tokens minted for steps | Cut 9 and [ADR 0007](adr/0007-integration-manifests.md) (token/PAT first; manifests execute no code) | Trigger and API access would come from one install, with no personal tokens |
| Multi-runner scale-out | Cut 2 and [ADR 0001](adr/0001-single-daemon.md) | The DB claim can move to `FOR UPDATE SKIP LOCKED` on Postgres behind the same interface |

## Prior art

| System | Borrow | Avoid |
|---|---|---|
| Drone CI | Env-var/file step contract; secret scrubbing | CI-specific YAML semantics |
| GitHub Actions | `uses:` + manifest idea | JS-runtime actions, marketplace complexity |
| n8n | Credential UX | A per-node SDK and bespoke UI code per integration |
| Argo Workflows | Container-native step model | A Kubernetes dependency |
| `act` / Dagger | — | Competing as general-purpose local CI ([ADR 0002](adr/0002-session-spine.md)) |
| ai-jail | Deny-by-default sandbox concepts: seccomp, rlimits, tmpfs home, secret masking | Its mechanism as-is — bubblewrap targets host processes, not containers |
| ai-launcher | Agent catalog, digest pinning, opt-in capabilities, dry-run (`validate_workflow`) | Desktop toggles (GPU/display) |

## Residual risks

- **Credentials live inside the agent container** ([ADR 0005](adr/0005-persistent-home-egress.md)). The blast radius of a prompt-injected agent is bounded by the mandatory egress allowlist, not by withholding the secret. Any allowlisted endpoint is a usable channel for a compromised agent (quota burn, calls to the provider). The credential-injecting proxy remains the desired end-state for untrusted profiles.
- **Polling watch has a CPU cost** that multiplies by the number of active sessions — a direct input to O1.
- **Cross-platform UID / bind-mount ownership** diverges between macOS (VM-handled) and Linux (real UID mismatch); the Phase 1 "preserves UID" acceptance test must cover both. User-namespace remapping or a rootless engine would stop root in a container from being root on the host, but on Linux it also shifts the ownership of files written into the bind-mounted worktree; whether and how to adopt it is undecided.
