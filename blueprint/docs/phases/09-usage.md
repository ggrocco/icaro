# Phase 9 — Unified usage (nice-to-have)

**Objective:** collect provider usage without fragility, using codeburn-style parsing of local agent logs. This is a cost tracker, not a compute limiter ([decision index](../02-decisions-risks.md#decision-index)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Define a usage provider interface.
- [ ] Start with explicit parsing of supported sources (local agent logs such as `~/.claude/`).
- [ ] Save snapshots and expose limits/alerts.
- [ ] Add a usage UI.
- [ ] Never infer cost as fact without provider metadata.

## Acceptance criteria

- [ ] An unavailable provider does not break an agent run.
- [ ] Data shows its source and confidence.
- [ ] Alerts respect the configured timezone and limit.
