# Phase 3 — Integrations and notifications

## Objective

Objective: the integration manifest system (ADR 0007) — git-backed packs, generic webhook triggers, in-daemon HTTP actions — plus cron and the repository cache. GitHub, GitLab, Slack, Jira, Linear, Sentry, and Datadog ship as bundled manifests.

## Dependencies

Depends on Phase 2 (interpolation engine, `when` grammar, executor); the repository cache can be prepared alongside Phase 1.

## Deliverables

- [ ] Implement a repository cache keyed by ref/revision.
- [ ] Add a persistent scheduler and a concurrency policy.
- [ ] Define and version the **manifest schema** (triggers: verify/match/payload normalization; actions: typed inputs, HTTP request template, auth reference, output extraction).
- [ ] Implement **pack management**: embedded read-only `builtin` pack; `icaro integration add/update/push` over git; auto-initialized `local` pack; shadow rules (user pack may shadow builtin, user/user collision is an error); watch + reload keeping the last good version of an invalid manifest.
- [ ] Implement the **generic webhook endpoint** (`/hooks/<integration>`): manifest-driven signature verification, dedup by delivery id, body caps, rate limiting, per-workflow URL token for unsigned webhooks.
- [ ] Implement the **`${{ trigger.* }}` context**: payload normalization, `filter` evaluation, and manual-run prompting for payload fields.
- [ ] Implement the **HTTP action executor** for `uses:` steps: in-process execution, typed-input validation, output extraction, host-bound credentials, SSRF guard (private ranges blocked by default, per-integration opt-in), response-body redaction.
- [ ] Add **host-bound integration credentials** to the daemon's secret store, confirmed at `icaro credential add`; pack-install permission summary (hosts, auth kinds).
- [ ] Author and validate the seven **bundled manifests** (GitHub, GitLab, Slack, Jira, Linear, Sentry, Datadog) against the live APIs.

## Acceptance criteria & manual tests

- [ ] The same repo is not cloned twice within a single run.
- [ ] A repeated webhook does not duplicate execution.
- [ ] A cron missed during a restart is reconciled.
- [ ] Secrets do not appear in logs, including HTTP response bodies from actions.
- [ ] `icaro integration add <git-url>` makes a third-party pack's triggers and actions available without rebuilding the binary.
- [ ] A step consumes `${{ trigger.pr_number }}` from a GitHub webhook; the same workflow run manually prompts for the field.
- [ ] The executor refuses to attach a credential to a host outside its binding, and refuses a private-range URL without the opt-in.
- [ ] Committing a broken manifest keeps the last good version serving; the error surfaces in status/UI.

## Automated tests

- Unit: business logic and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: only critical flows, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not advance until the criteria above are green. Record new decisions in `docs/adr/`.
