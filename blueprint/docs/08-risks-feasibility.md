# Risks & feasibility gate

This document records the decisions taken to de-risk the blueprint and the
spikes that **must be answered before Phase 0 build** (not inside the phase that
depends on them). It complements `docs/02-decisions-risks.md`; where the two
disagree, this document is newer.

## Decisions taken

| # | Area | Decision | Effect on prior docs |
|---|---|---|---|
| D1 | Previews — propagation | The project is **bind-mounted, not copied**. Hot-reload is the **app dev server's** job, not Ícaro's. Ícaro only **restarts on configuration change** (compose/env/lockfile). | Narrows architecture §7 scope |
| D2 | Previews — topology | The agent and the app dev server run in **separate containers**, sharing the same host worktree path. | Confirms architecture §7 |
| D3 | Previews — macOS | Because of D2, on Docker Desktop the preview's in-container watcher will **not** reliably receive inotify events; **polling watch is mandatory on macOS** (`CHOKIDAR_USEPOLLING`, Vite `server.watch.usePolling`, etc.). Data stays coherent across the two VirtioFS hops; only events are unreliable. | Resolves the §7 spike |
| D4 | Credentials | Replace the "broker by default" stance with a **persistent per-profile home** (seeded once at login, reused across steps and the dev container), protected by a **mandatory egress allowlist**. See **ADR 0005**. | Supersedes the default in ADR 0003 |
| D5 | Credentials — concurrency | The persistent home is a **golden seed layer** (holds creds) + a **per-session writable overlay** (runtime writes), so login is shared but concurrent sessions never corrupt one `~/.claude`. | New; see ADR 0005 |
| D6 | Session state | Add a first-class **`sessions` table** in SQLite + a **startup reconciliation** pass. See **ADR 0006**. | Fills the gap in architecture §4 |
| D7 | Usage / cost | Adopt **codeburn-style local log parsing** (reads `~/.claude/` etc.) for Phase 9 usage. It is a **cost tracker, not a compute limiter** — it does not bound RAM/CPU/containers. | Scopes Phase 9 |

## Open item needing confirmation

| Area | Proposed default | Why it matters |
|---|---|---|
| Compute budget for N parallel sessions | A **configurable concurrency cap** (active sessions ≤ K, the rest enter a `queued` state) plus per-container `--memory`/`--cpus` limits. Default K derived from detected RAM. | The wedge is "N agents in parallel," but each session is an agent container **+** a preview compose stack. Docker Desktop's fixed VM memory ceiling is hit fast on a laptop. codeburn (D7) does **not** address this — it tracks dollars, not compute. |

## Week-0 feasibility gate (run before Phase 0 build)

Per the project rule that doubts are resolved up front, these spikes gate the
build. Each has a pass criterion and a fallback.

| Spike | Question to answer | Pass criterion | Fallback if it fails |
|---|---|---|---|
| **S1 — macOS file propagation** | With agent and preview in **separate** containers on the **same** host worktree (D2), does the preview dev server reflect an agent's edit on Docker Desktop? | An agent-container edit produces a visible preview change in < ~3 s with **polling** enabled (D3); VirtioFS keeps data coherent. | Document host-run dev servers as the macOS path; native inotify is not promised. |
| **S2 — per-session home overlay** | Can a golden home (creds) + per-session writable overlay (D5) be mounted into the agent container on Docker Desktop and Linux? | Two concurrent sessions share the seeded login yet write to isolated layers with no `~/.claude` corruption. | Per-session full-copy home (more disk, slower start) or serialize sessions sharing a profile. |
| **S3 — MCP transport into the container** | "MCP over the session Unix socket" (ADR 0004) is not a standard MCP transport — agent CLIs expect stdio or HTTP. How is it actually injected and reached from **inside** the container? | A target CLI (Claude Code/Codex) reads/writes memory via an injected MCP config that bridges to the daemon, with scope derived from session identity. | A small stdio bridge binary baked into the agent image that proxies to the bind-mounted session socket. |
| **S4 — credential reality per agent** | Does each target agent CLI actually authenticate from the persistent home alone (D4), with **no** usable provider secret leaking into a place we don't control? | For each supported CLI, the only creds present are in the seeded home, and the **mandatory egress allowlist** confines their use. | Per-agent adapter; downgrade unsupported agents to a documented manual-login profile. |
| **S5 — microVM isolation (optional, time-boxed)** | Is per-container microVM isolation (Apple Containerization / gVisor / Firecracker) viable for untrusted profiles? | A spike-quality prototype runs an agent under the stronger boundary. | Mandatory egress allowlist remains the boundary for untrusted profiles. |

## Residual risks carried forward

- **Credentials live inside the agent container** (D4). The blast radius of a
  prompt-injected agent is now bounded by the **mandatory egress allowlist**,
  not by withholding the secret. Any allowlisted endpoint is a usable channel
  for a compromised agent (quota burn, calls to the provider). The
  credential-injecting proxy in ADR 0005 remains the desired end-state for
  untrusted profiles.
- **Polling watch has a CPU cost** (D3) that multiplies by the number of active
  sessions — a direct input to the concurrency cap above.
- **Workflow `step = image` vs. `step drives a session`** tension (architecture
  §6 / Phase 2) and the **unwritten `when` expression grammar** remain to be
  specified before the executor is built.
- **Cross-platform UID / bind-mount ownership** diverges between macOS
  (VM-handled) and Linux (real UID mismatch); the "preserves UID" acceptance
  test must cover both.
