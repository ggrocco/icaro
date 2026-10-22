# Phase 9 — Unified usage (nice-to-have)

**Objective:** collect provider usage without fragility, using codeburn-style parsing of local agent logs. This is a cost tracker, not a compute limiter ([decision index](../02-decisions-risks.md#decision-index)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Define a usage provider interface.
- [x] Start with explicit parsing of supported sources (local agent logs such as `~/.claude/`).
- [x] Save snapshots and expose limits/alerts (`usage_snapshots`, `usage.daily_limit_usd`).
- [x] Add a usage UI. *The CLI view (`icaro usage`, `--json`) and the web UI's **Usage** page over `GET /api/v1/usage` (covered by `web/e2e`).*
- [x] Never infer cost as fact without provider metadata.

## Acceptance criteria

- [x] An unavailable provider does not break an agent run.
- [x] Data shows its source and confidence.
- [x] Alerts respect the configured timezone and limit.

Decisions: [ADR 0022](../adr/0022-usage-from-local-logs.md). Only Claude Code transcripts are parsed so far; other CLIs wait on spike S10.
