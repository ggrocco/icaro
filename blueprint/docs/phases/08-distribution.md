# Phase 8 — Distribution and updates

## Objective

Objective: installer, Brew, and secure auto-update.

## Dependencies

Can start after Phase 0; only finalize after the app/CLI.

## Deliverables

- [ ] Create install.sh that verifies architecture, checksum, and signature.
- [ ] Create a Homebrew tap/formula.
- [ ] Create a release pipeline with SBOM, checksums, and signature.
- [ ] Implement updates with staging and rollback.
- [ ] Document config upgrade/migration.

## Acceptance criteria & manual tests

- [ ] A fresh install works on macOS and supported Linux.
- [ ] An invalid checksum blocks the installation.
- [ ] An update preserves config and DB.
- [ ] Rollback restores the previous binary.

## Automated tests

- Unit: business rules and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: critical flows only, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not move forward until the criteria above are green. Record any new decisions in `docs/adr/`.
