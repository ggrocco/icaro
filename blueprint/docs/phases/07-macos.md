# Phase 7 — macOS app

## Objective

Objective: Wails, tray, startup, and native UX.

## Dependencies

Depends on Phase 0; can use a mock server until Phase 2/5.

## Deliverables

- [ ] Connect Wails to the existing daemon.
- [ ] Add a menu bar: previews, runs, usage, and quick actions.
- [ ] Configure launch at login and tray-only mode.
- [ ] Define a fallback/headless mode when the UI fails.
- [ ] Signing/notarization in the release pipeline.

## Acceptance criteria & manual tests

- [ ] Opening the app while the daemon is running does not create a second daemon.
- [ ] The tray shows the correct state after a restart.
- [ ] Launch at login works and does not open a window when configured.
- [ ] Smoke test on a clean macOS install.

## Automated tests

- Unit: business rules and validations, without real Docker.
- Integration: Docker/daemon/DB in an ephemeral environment.
- E2E: critical flows only, run in a separate CI.
- Regression: every bug produces a test before the fix.

## Review checkpoint

Do not move forward until the criteria above are green. Record any new decisions in `docs/adr/`.
