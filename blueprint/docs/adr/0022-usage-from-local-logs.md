# ADR 0022 — Usage comes from local agent logs, with source and confidence

**Status:** Accepted

## Context

Phase 9 wants unified usage without fragility: an unavailable provider must not break an agent run, every number must show its source and confidence, and cost must never be inferred as fact (spike S10). Provider billing APIs differ, need extra credentials, and do not cover subscription plans.

## Decision

- `internal/usage` defines a `Provider` interface (`Collect(since, timezone) → records`). The first provider parses **Claude Code JSONL transcripts** under each profile home (`~/.icaro/home/<profile>/**/.claude/projects/**`). The user's own `~/.claude` is read only with `--host-claude`, and read-only.
- A record is `(provider, source, day, model)` with input, output, cache-read and cache-write tokens and a message count. Responses that span several transcript lines are counted once (`message.id` + `requestId`). Token counts come from the provider's own usage fields: confidence `high`.
- **Cost is an estimate.** It is computed from a list-price table keyed by model prefix and labelled as an estimate everywhere. Models without an entry get no cost — never a guess. Alerts that include unpriced usage are marked partial ("at least").
- A provider error or panic becomes an `unavailable` row. Collection runs from the CLI (`icaro usage`) and never sits on an agent's code path.
- Days are bucketed in `usage.timezone`. Alerts fire when a day's estimated spend reaches `usage.daily_limit_usd`. Snapshots are upserted into `usage_snapshots`, so recollecting a window replaces its rows instead of double-counting them.

## Consequences

- Usage works offline and without provider credentials, and it covers subscription use that billing APIs would miss.
- The numbers are only as complete as the local logs. A transcript deleted before collection is lost, and other agents (Codex, Copilot) need their own providers once S10 confirms their log formats.
- The pricing table needs maintenance. Being stale only affects estimates, never token counts.
