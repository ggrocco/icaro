# Phase 7 — macOS app

**Objective:** Wails, tray, startup, and native UX.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. It may use a mock server for runs until Phase 2 lands.

## Deliverables

- [ ] Connect Wails to the existing daemon.
- [ ] Add a menu bar: previews, runs, usage, and quick actions.
- [ ] Configure launch at login and tray-only mode.
- [ ] Define a fallback/headless mode when the UI fails.
- [ ] Signing/notarization in the release pipeline.

## Acceptance criteria

- [ ] Opening the app while the daemon is running does not create a second daemon.
- [ ] The tray shows the correct state after a restart.
- [ ] Launch at login works and does not open a window when configured.
- [ ] Smoke test on a clean macOS install.
