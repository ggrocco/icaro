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

## Existing code

The Go code in the repository was built from the superseded `docs/` plan ([ADR 0008](adr/0008-blueprint-supersedes-engine-design.md)). It already provides the CLI, config, migrations, an API, and a container executor, but under the old model. Each phase migrates the rows assigned to it:

| Area | Code today | Blueprint target | Phase |
|---|---|---|---|
| Process & transport | `icaro serve --role all\|server\|runner`; REST on TCP `127.0.0.1:8787` | One daemon with lock/socket/PID; Unix socket by default, TCP only with `--listen` + token | 0 |
| Secret storage | `master.key` file or `ICARO_MASTER_KEY` | OS keychain / secret store; encrypted-file fallback only after an explicit password | 1 |
| Workspace & sessions | Per-run named volume at `/workspace`; runs re-attach by container label | Session worktree bind mount; `sessions` table + startup reconciliation | 1 |
| Sandbox selection | Per-step `sandbox: standard\|strict`, `network: egress\|none` | Per-profile policy with a mandatory egress allowlist (O2) | 1–2 |
| Step forms | `run` (script/command) and `http` | `image` + `command`; `uses:` integration actions | 2 (`image`), 3 (`uses`) |
| Interpolation & gating | Go `text/template` `{{ }}`, `if:` | `${{ }}`, `when` (O3) | 2 |
| Step graph & retry | Linear sequence, `continue_on_error`, `retry: {attempts, backoff}` | DAG with `needs`; integer `retry` per the schema | 2 |
| Schema | Generated `schema/workflow.schema.json` | Generated output equal to the [blueprint contract](schemas/workflow.schema.json) (drift test) | 2 |
| Triggers | `on.webhook` at `/hooks/{workflow}/{token}` with the raw body as input; `on.schedule` | `triggers:` (`manual`, `cron`, integration events) at `/hooks/<integration>` with normalized `trigger.*` | 3 |
| Credentials | Named connections injected as `ICARO_CONN_*`; `http` auth from a connection | `${{ secrets.* }}`; host-bound integration credentials; agent creds in the profile home | 1 (home), 3 (integrations) |

Kept as is: the SQLite + Postgres store with portable SQL and per-dialect migrations, the sandbox hardening flags, log scrubbing, hashed API tokens, run-lineage columns, and the image-step I/O contract (`hack/spike-io` checks it against a read-only root filesystem).

## Using simpler models

- **Mechanical tasks:** creating scaffolding, table-driven tests, fixtures, API docs, migrations, repetitive components, and lint fixes. Use a cheap/fast model.
- **Critical tasks:** threat model, concurrency model, Docker security policy, DAG executor, schema evolution, post-crash recovery, and releases. Use a more capable model with mandatory human review.
- **Rule:** the simple model does not decide architecture or change security boundaries; it implements tasks whose acceptance criteria are already locked down.
