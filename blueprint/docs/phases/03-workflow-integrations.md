# Phase 3 — Integrations and notifications

## Objective

Objective: git, cron, webhooks, Slack/email.

## Dependencies

Depends on Phase 2; the git integration can be prepared alongside Phase 1.

## Deliverables

- [ ] Implement a repository cache keyed by ref/revision.
- [ ] Add a persistent scheduler and a concurrency policy.
- [ ] Create an HMAC webhook endpoint with deduplication.
- [ ] Implement GitHub/GitLab adapters.
- [ ] Add `notify` with safe templates.

## Acceptance criteria & manual tests

- [ ] The same repo is not cloned twice within a single run.
- [ ] A repeated webhook does not duplicate execution.
- [ ] A cron missed during a restart is reconciled.
- [ ] Secrets do not appear in logs.

## Automated tests

- Unit: business logic and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: only critical flows, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not advance until the criteria above are green. Record new decisions in `docs/adr/`.
