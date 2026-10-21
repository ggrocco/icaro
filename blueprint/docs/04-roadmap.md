# Roadmap

## Critical path: the session spine

The wedge is parallel isolated agents ([ADR 0002](adr/0002-session-spine.md)), so the **critical path is the session spine**, not independent tracks. The first shippable milestone is one working session end-to-end:

> create a worktree → run an agent in an isolated container with its profile home and egress allowlist → reach a live preview → read/write scoped memory over MCP.

Phases 1, 5, and 6 converge on this milestone; they are facets of the spine, not parallel products. Breadth work (workflows, canvas UI, native app, distribution) follows once a single session works. The [week-0 spikes](02-decisions-risks.md#feasibility-gate) run before Phase 0.

## Dependencies

```text
0 ── 1 ─┬─ 5 ─┬─ SPINE ─┬─ 2 ── 3 ── 4
        └─ 6 ─┘         ├─ 7 ── 8
                        └─ 9  (optional)
0 ── 10   (runs alongside everything; wraps up at release)

SPINE + 2 + 4 + 7 + 8 + 10 ──> 11
```

| Phase | Track | Depends on |
|---|---|---|
| [0 — Foundation & contracts](phases/00-foundation.md) | Spine | week-0 spikes; blocks every other phase |
| [1 — Container and CLI](phases/01-container-cli.md) | Spine | 0 |
| [5 — Previews per worktree](phases/05-previews.md) | Spine | 1 |
| [6 — Shared memory](phases/06-memory.md) | Spine | 1 (in parallel with 5) |
| [2 — Workflow core](phases/02-workflow-core.md) | Automation | spine |
| [3 — Integrations](phases/03-workflow-integrations.md) | Automation | 2 |
| [4 — Workflow UI and agent authoring](phases/04-workflow-ui.md) | Automation | 3 |
| [7 — macOS app](phases/07-macos.md) | Platform | spine (MVP cut 7) |
| [8 — Distribution and updates](phases/08-distribution.md) | Platform | 7 |
| [9 — Unified usage](phases/09-usage.md) | Optional | spine; not a GA gate |
| [10 — Documentation and landing page](phases/10-docs-landing.md) | Product/docs | 0; documentation tracks the real APIs |
| [11 — Hardening and GA](phases/11-hardening.md) | Quality | spine + 2 + 4 + 7 + 8 + 10 |

## Rules for every phase

Each phase file lists only its objective, deliverables, and acceptance criteria. These rules apply to all of them:

- **Entry:** the phase's dependencies are done, its phase-gated spikes in the [feasibility gate](02-decisions-risks.md#feasibility-gate) have passed, and no [open item](02-decisions-risks.md#open-items) blocks it.
- **Tests:** deliverables ship with unit, integration, E2E (critical flows only), and regression tests as defined in [05-engineering](05-engineering.md#test-strategy).
- **Exit (review checkpoint):** do not advance until every acceptance criterion is green.
- **Decisions:** record new decisions as an ADR in [`adr/`](adr/) and add a row to the [decision index](02-decisions-risks.md#decision-index).

## Status

State of each phase against its acceptance criteria. Each phase file ticks what is done and annotates what is open.

| Phase | State | Open |
|---|---|---|
| [0 — Foundation](phases/00-foundation.md) | Done | Domain coverage is below the 95% target in several packages; exceptions are not yet justified one by one |
| [1 — Container and CLI](phases/01-container-cli.md) | Done on Linux | The macOS half of "`icaro sh` preserves the UID" and the Keychain backend have not run on a Mac; the agent base image is built and pushed by the release workflow but no release has run yet |
| [2 — Workflow core](phases/02-workflow-core.md) | Done | — |
| [3 — Integrations](phases/03-workflow-integrations.md) | Done | Spike S8: the bundled manifests are checked against documented payload fixtures, not the live APIs |
| [4 — Workflow UI and agent authoring](phases/04-workflow-ui.md) | Done | The run graph is read-only; drag/drop editing is deferred, as planned |
| [5 — Previews](phases/05-previews.md) | Done on Linux | An agent-container edit reaching the preview is verified on Linux (`hack/e2e`); macOS is not (spike S1); DNS-01/TLS is deferred |
| [6 — Shared memory](phases/06-memory.md) | Done | The importers are prototypes over assumed export formats |
| [7 — macOS app](phases/07-macos.md) | Partial | Only launch at login (`icaro service install`) is done. The Wails app, tray, notifications, tray-only mode, Apple signing/notarization and the clean-install smoke test need macOS hardware (spike S9) |
| [8 — Distribution](phases/08-distribution.md) | Done on Linux | A fresh install on macOS |
| [9 — Usage](phases/09-usage.md) (optional) | Done | Only Claude Code transcripts are parsed; other CLIs wait on spike S10 |
| [10 — Docs and landing page](phases/10-docs-landing.md) | Done | — |
| [11 — Hardening and GA](phases/11-hardening.md) | Done on Linux | macOS/Docker Desktop smoke tests and the public beta; a real Docker daemon restart is simulated, not exercised ([ADR 0024](adr/0024-hardening-budget.md) records the budget) |

Open spikes:

- S1: macOS file propagation.
- S4: every agent CLI authenticates from the seeded home alone. This needs real logins.
- S5: microVM isolation (optional).
- S6: heterogeneous compose projects.
- S7: `.test` resolution with VPNs.
- S8: manifests against the live APIs.
- S9: Wails v3.

S2 (home overlay, on Linux) and S3 (MCP transport, [ADR 0019](adr/0019-memory-mcp-transport.md)) have passed.

## Existing code

The first Go code was built from the superseded `docs/` plan ([ADR 0008](adr/0008-blueprint-supersedes-engine-design.md)). Each phase migrated the rows assigned to it; every row is now migrated:

| Area | Code today | Blueprint target | Phase |
|---|---|---|---|
| Process & transport | **Done (Phase 0):** one daemon per user (`internal/daemon`: flock lock, PID, `0600` Unix socket); TCP only with `--listen` and a token, plus the on-demand loopback UI listener ([ADR 0023](adr/0023-browser-access.md)) | One daemon with lock/socket/PID; Unix socket by default, TCP only with `--listen` + token | 0 |
| Secret storage | **Done (Phase 1):** master key in the macOS Keychain / Secret Service, password-protected file fallback; `ICARO_MASTER_KEY` stays as a headless override | OS keychain / secret store; encrypted-file fallback only after an explicit password | 1 |
| Workspace & sessions | **Done (Phases 1–2):** session worktrees bind-mounted at `/workspace`, `sessions` table with startup reconciliation, one run session per workflow run ([ADR 0010](adr/0010-run-sessions.md)) | Session worktree bind mount; `sessions` table + startup reconciliation | 1 |
| Sandbox selection | **Done (Phases 1–2):** per-profile egress allowlist on an internal network with a proxy ([ADR 0012](adr/0012-egress-proxy-and-home-layers.md)); workflow steps use `workflows.profile` | Per-profile policy with a mandatory egress allowlist (O2) | 1–2 |
| Step forms | **Done (Phase 2):** `image` + `command`; `uses` steps run through the integration action executor | `image` + `command`; `uses:` integration actions | 2 (`image`), 3 (`uses`) |
| Interpolation & gating | **Done (Phase 2):** `${{ }}` and `when` ([ADR 0009](adr/0009-expression-language.md)) | `${{ }}`, `when` (O3) | 2 |
| Step graph & retry | **Done (Phase 2):** DAG with `needs`, integer `retry` ([ADR 0013](adr/0013-workflow-execution-semantics.md)) | DAG with `needs`; integer `retry` per the schema | 2 |
| Schema | **Done (Phase 2):** generated from the Go types, drift-tested against the contract | Generated output equal to the [blueprint contract](schemas/workflow.schema.json) (drift test) | 2 |
| Triggers | ~~`on.webhook` at `/hooks/{workflow}/{token}` with the raw body as input; `on.schedule`~~ Done: `internal/integration` serves `/hooks/<integration>` and `/hooks/webhook/<token>` with normalized `trigger.*`, and `internal/scheduler` fires `cron` triggers ([ADR 0015](adr/0015-manifest-schema-webhook-pipeline.md), [0016](adr/0016-pack-loading-action-execution.md)) | `triggers:` (`manual`, `cron`, integration events) at `/hooks/<integration>` with normalized `trigger.*` | 3 |
| Credentials | ~~Named connections injected as `ICARO_CONN_*`~~ (removed). Done: Phase 1 stores agent credentials in profile homes and Phase 2 resolves `${{ secrets.* }}` in image steps; Phase 3: `icaro credential add` stores host-bound `<integration>.<name>` secrets used only by the action executor | `${{ secrets.* }}`; host-bound integration credentials; agent creds in the profile home | 1 (home), 3 (integrations) |

Kept as is: the SQLite + Postgres store with portable SQL and per-dialect migrations, the sandbox hardening flags, log scrubbing, hashed API tokens, run-lineage columns, and the image-step I/O contract (`hack/spike-io` checks it against a read-only root filesystem).

## Using simpler models

- **Mechanical tasks:** creating scaffolding, table-driven tests, fixtures, API docs, migrations, repetitive components, and lint fixes. Use a cheap/fast model.
- **Critical tasks:** threat model, concurrency model, Docker security policy, DAG executor, schema evolution, post-crash recovery, and releases. Use a more capable model with mandatory human review.
- **Rule:** the simple model does not decide architecture or change security boundaries; it implements tasks whose acceptance criteria are already locked down.
