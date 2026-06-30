# Phase 9 — Unified usage (nice-to-have)

## Objective

Objective: collect provider usage without fragility.

## Dependencies

After the core is stable.

## Deliverables

- [ ] Define a usage provider interface.
- [ ] Start with explicit parsing of supported sources.
- [ ] Save snapshots and expose limits/alerts.
- [ ] Add a usage UI.
- [ ] Never infer cost as fact without provider metadata.

## Acceptance criteria & manual tests

- [ ] An unavailable provider does not break an agent run.
- [ ] Data shows its source and confidence.
- [ ] Alerts respect the configured timezone and limit.

## Automated tests

- Unit: business rules and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: critical flows only, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not move forward until the criteria above are green. Record any new decisions in `docs/adr/`.
