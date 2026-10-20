# Phase 7 — macOS app

**Objective:** Wails, tray, startup, and native UX.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. It may use a mock server for runs until Phase 2 lands.

## Deliverables

- [ ] Connect Wails to the existing daemon.
- [ ] Add a menu bar: previews, running/queued/failed runs (opening the run in the UI), usage, and quick actions (run, cancel).
- [ ] Notify on run failure and when the daemon becomes unreachable.
- [ ] Configure launch at login (a launchd user agent) and tray-only mode. *Launch at login is done (`icaro service install`, [`internal/autostart`](../../../internal/autostart/)); tray-only mode needs the Wails app.*
- [x] Define a fallback/headless mode when the UI fails: the daemon runs headless under launchd/systemd (`icaro service install`), and the CLI and the daemon-served web UI need no native app.
- [ ] Signing/notarization in the release pipeline. *Release signing (ed25519 + cosign) is done ([ADR 0021](../adr/0021-release-signing-and-updates.md)); Apple code signing and notarization need a Developer ID and a macOS runner.*

## Acceptance criteria

- [ ] Opening the app while the daemon is running does not create a second daemon.
- [ ] The tray shows the correct state after a restart.
- [ ] Launch at login works and does not open a window when configured. *The launchd agent runs `icaro serve` with no window and is unit-tested (generated plist, `launchctl bootstrap`); it still needs a run on a real Mac.*
- [ ] Smoke test on a clean macOS install.

## Status

Done without a Mac: the launchd user agent and systemd user unit (`icaro service install|uninstall|status`), and the headless fallback. Remaining: the Wails app (spike S9), the tray and menu bar, native notifications, notarization, and the clean-install smoke test, which all need macOS hardware.
