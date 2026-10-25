# Phase 7 — macOS app

**Objective:** Wails, tray, startup, and native UX.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. It may use a mock server for runs until Phase 2 lands.

## Deliverables

- [x] Connect Wails to the existing daemon. *The app attaches to the daemon over its socket, or starts `icaro serve` when none answers, and runs no engine of its own ([ADR 0027](../adr/0027-desktop-app.md)). The logic is in [`internal/desktop`](../../../internal/desktop/), the Wails shell in [`desktop/`](../../../desktop/).*
- [x] Add a menu bar: previews, running/queued/failed runs (opening the run in the UI), usage, and quick actions (run, cancel).
- [x] Notify on run failure and when the daemon becomes unreachable.
- [x] Configure launch at login and tray-only mode. *The daemon's launchd user agent is `icaro service install` ([`internal/autostart`](../../../internal/autostart/)). The app's own login item comes from the menu (SMAppService on macOS 13+). Tray-only mode is a saved setting, so a login launch opens no window.*
- [x] Define a fallback/headless mode when the UI fails: the daemon runs headless under launchd/systemd (`icaro service install`), and the CLI and the daemon-served web UI need no native app.
- [x] Signing/notarization in the release pipeline. *The release job builds a universal `Icaro.app`, signs it with a Developer ID and the hardened runtime, then notarizes and staples it. It needs the `MACOS_*`/`APPLE_*` secrets; without them it signs ad hoc and posts a warning.*

## Acceptance criteria

- [x] Opening the app while the daemon is running does not create a second daemon. *Unit-tested, and checked on Linux with a real daemon. The app starts one only after a failed health check, and the daemon's lock settles a race.*
- [x] The tray shows the correct state after a restart. *Each poll rebuilds the state from the API, and nothing is cached across restarts. Run failures that happen while the daemon is down are still notified.*
- [ ] Launch at login works and does not open a window when configured. *Tray-only is unit-tested, and the daemon's launchd agent is unit-tested (generated plist, `launchctl bootstrap`). The app's login item needs a run on a real Mac.*
- [ ] Smoke test on a clean macOS install.

## Status

Built and tested without a Mac:
- the app (CI builds and tests it on macOS and Linux, and assembles an ad-hoc-signed `Icaro.app` on macOS);
- the menu, notifications, actions and tray-only mode;
- the release job that signs and notarizes the bundle.

On Linux under Xvfb with a real daemon:
- the app attached to a running daemon without starting another;
- it started one when none answered;
- a second launch handed over to the first instance;
- quitting left the daemon running.

Remaining:
- the clean-install smoke test on a Mac (spike S9): menu bar, notification permission, login item, Gatekeeper;
- an Apple Developer ID for the release secrets.
