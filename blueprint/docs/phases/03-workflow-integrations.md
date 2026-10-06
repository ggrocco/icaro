# Phase 3 — Integrations

**Objective:** the integration manifest system ([ADR 0007](../adr/0007-integration-manifests.md), [architecture §9](../01-architecture.md#9-integrations)) — git-backed packs, generic webhook triggers, in-daemon HTTP actions — plus cron and the repository cache. GitHub, GitLab, Slack, Jira, Linear, Sentry, and Datadog ship as bundled manifests; notifications are `uses:` actions.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Implement a repository cache keyed by ref/revision.
- [ ] Add a persistent scheduler and a concurrency policy.
- [ ] Define and version the **manifest schema** (triggers: verify/match/payload normalization; actions: typed inputs, HTTP request template, auth reference, output extraction).
- [ ] Implement **pack management**: embedded read-only `builtin` pack; `icaro integration add/update/push` over git; auto-initialized `local` pack; shadow rules; watch + reload keeping the last good version of an invalid manifest.
- [ ] Implement the **generic webhook endpoint** (`/hooks/<integration>`) enforcing the [webhook policy](../03-security.md#mandatory-policies).
- [ ] Implement the **`${{ trigger.* }}` context**: payload normalization, `filter` evaluation, and manual-run prompting for payload fields.
- [ ] Implement the **HTTP action executor** for `uses:` steps: in-process execution, typed-input validation, and output extraction, under the [integration policies](../03-security.md#mandatory-policies) (host-bound credentials, SSRF guard, response-body redaction).
- [ ] Add **host-bound integration credentials** to the daemon's secret store, confirmed at `icaro credential add`, and the pack-install permission summary.
- [ ] Author the seven **bundled manifests** and validate them against the live APIs (S8).

## Acceptance criteria

- [ ] The same repo is not cloned twice within a single run.
- [ ] A repeated webhook does not duplicate execution.
- [ ] A cron missed during a restart is reconciled.
- [ ] Secrets do not appear in logs, including HTTP response bodies from actions.
- [ ] `icaro integration add <git-url>` makes a third-party pack's triggers and actions available without rebuilding the binary.
- [ ] A step consumes `${{ trigger.pr_number }}` from a GitHub webhook; the same workflow run manually prompts for the field.
- [ ] The executor refuses to attach a credential to a host outside its binding, and refuses a private-range URL without the opt-in.
- [ ] Committing a broken manifest keeps the last good version serving; the error surfaces in status/UI.
