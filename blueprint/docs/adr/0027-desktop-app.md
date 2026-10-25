# ADR 0027 — The desktop app: a Wails v3 tray over the daemon

**Status:** Accepted. Spike S9 is answered for building and running the app; the clean-install pass still needs a Mac.

## Context

[Phase 7](../phases/07-macos.md) asks for a native app with:
- a menu bar listing previews, runs and usage, with quick actions;
- notifications;
- launch at login and a tray-only mode;
- Apple signing and notarization.

[Architecture §1](../01-architecture.md#1-processes--ownership) says the app starts or attaches to the daemon and never runs a second engine. [§3](../01-architecture.md#3-transport-between-ui-cli-and-daemon) keeps operational data on the same HTTP contract as the CLI.

[Spike S9](../02-decisions-risks.md) asks whether Wails v3 is mature enough for this. Wails v3 is still a beta (`v3.0.0-beta.34` when this was written), and Wails v2 has no system tray at all.

## Decision

**Wails v3, pinned to an exact version.** It is the only Wails line with a system tray, a notification service, single-instance launch and a login-item API (SMAppService). Upgrades are deliberate.

The fallback in the spike table, "Wails v2 behind `internal/platform`", does not work, because v2 has no tray. If v3 fails on a Mac, two things remain:
- the headless mode that already ships;
- another tray library behind the same `desktop.UI` interface.

**Two layers.**
- `internal/desktop` is in the main module, in pure Go with no cgo, and is gated at 95% coverage like every package. It holds all the logic:
  - attaching to or starting the daemon;
  - polling;
  - the menu model;
  - the notification rules;
  - the actions.
- `desktop/` is its own Go module (`icaro/desktop`) and needs cgo. It holds the Wails shell, which renders the menu model and passes clicks back.

The separate module keeps cgo, GTK/WebKit and Wails' dependencies out of the CLI's build, `go test ./...`, its license list and govulncheck.

`internal/desktop` is UI glue rather than domain code. Its controller state (the last poll, the settings, the cached usage) is read by menu clicks on the shell's threads and written by the poll loop, so a single mutex guards it. That is the one exception to the engineering rule of channels over mutexes, recorded here as that rule requires. The app has its own `desktop/THIRD_PARTY_LICENSES.md`, generated for the macOS build.

**One daemon.** The app resolves the configuration as the CLI does: the same `--config`/`--home` flags, `ICARO_HOME` and `daemon.url`.
- It checks the socket's health and starts `icaro serve` (`daemon.StartProgram`) only when nothing answers. The daemon's lock settles a race with the CLI or with launchd.
- It never starts a TCP daemon.
- It never stops the daemon: quitting the app leaves runs running.

The `icaro` binary is found in this order:
1. `$ICARO_BIN`;
2. the copy bundled in `Icaro.app/Contents/MacOS`;
3. `/opt/homebrew/bin`, `/usr/local/bin` and `~/.local/bin`;
4. `PATH`.

The fixed directories are there because a GUI app on macOS does not inherit the shell's `PATH`.

**State comes from the API alone.**
- Every 3 s the app reads health, running/queued/failed runs, previews and active workflows.
- It reads usage once a minute, because usage parses transcripts.
- Nothing is cached across restarts. After either the app or the daemon restarts, the tray shows what the API says, which is the acceptance criterion "the tray shows the correct state after a restart".
- The menu is rebuilt only when it changes, so an open menu is not redrawn under the pointer.

**The menu** has these entries:
- the daemon's status, and **Start daemon** when a local daemon does not answer;
- **Open Ícaro**;
- active runs (open, cancel);
- recent failures (open);
- **Run workflow**: a workflow that needs no trigger fields runs directly; one that does opens in the UI so they can be filled in;
- previews (a running one opens its URL);
- today's usage (an estimate, "≥" when some usage is unpriced, and a flag when over the daily limit);
- **Launch at login**, **Tray only** and **Quit**.

Each section shows at most five entries, then "N more…". On macOS the icon carries the number of active runs, or `!` while the daemon is unreachable.

**Notifications:**
- A run that fails after the app started. Failures from before are history. A failure while the daemon was down still counts, for example a run reconciled as failed at restart. More than three new failures in one poll become one notification.
- The daemon being unreachable for two polls in a row, posted once, plus one when it is back. One slow answer is not an outage.

Clicking a notification opens the run, or starts the daemon. Only those non-destructive actions are accepted back from the notification center.

**The window.** **Open Ícaro** opens one window on the daemon's web UI, signed in with the same one-time link `icaro ui` mints ([ADR 0023](0023-browser-access.md)). Closing the window hides it. The app exposes no API of its own to the page. For a TCP daemon the window opens the UI's sign-in page.

**Launch at login and tray-only.**
- Launch at login is the platform's login item:
  - SMAppService for the bundled app on macOS 13+;
  - a LaunchAgent otherwise;
  - XDG autostart on Linux.

  The registration itself is the source of truth, toggled from the menu. It is separate from the daemon's agent (`icaro service install`), which still starts the daemon without the app.
- Tray-only mode is a saved setting (`~/.icaro/config/desktop.json`), not a flag, because SMAppService launches the app with no arguments. `--tray-only` forces it.
- A second launch shows the first instance's window.

**Bundle and release.**
- `Icaro.app` contains universal `icaro-app` and `icaro` binaries. It sets `LSUIElement` (no Dock icon), uses the bundle id `dev.icaro.app`, and builds its icon from `desktop/icons/app.png`.
- The release job builds the bundle on a macOS runner. With the Developer ID secrets, it signs with the hardened runtime and a timestamp, then notarizes and staples. Without them, it signs ad hoc and posts a warning.
- The zip ships with a SHA-256 file and the same ed25519 signature as `checksums.txt`.

**Linux.** The same app builds and runs on Linux (GTK 4, WebKitGTK 6; the tray needs a StatusNotifier host). It is not distributed: Linux stays CLI plus daemon, as the [blueprint](../../README.md) says. CI builds and tests it so the shell compiles on every push.

## Spike S9 so far

**Done:**
- CI builds and tests the app on macOS and Linux, and assembles an ad-hoc-signed `Icaro.app` on macOS.
- On Linux under Xvfb with a real daemon:
  - the app attached to a running daemon without starting another;
  - it started one when none answered;
  - a second launch handed over to the first instance, which opened its window;
  - quitting left the daemon running.

**Not done:** a run on a clean macOS install, covering:
- the menu bar icon and label;
- the notification permission prompt;
- the SMAppService login item;
- a notarized build passing Gatekeeper.

The pass criterion is unchanged.

## Consequences

- Wails is a beta. An upgrade can change its API, so the shell is kept small, and the logic it renders does not depend on Wails.
- CI gains a `desktop` job on two operating systems.
- Notarization needs an Apple Developer ID. Until the secrets are set, the release zip is signed ad hoc, and Gatekeeper blocks it after download (Control-click → Open).
- The release job now builds the web UI before goreleaser. Released binaries had been shipping without it and served the "not built" page, which the app's window would have shown.
