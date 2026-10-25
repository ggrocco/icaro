# Desktop app

`Icaro.app` is a menu bar app on top of the daemon. It shows what is running and what failed, gives you a few quick actions, and opens the web UI in its own window. It does no work of its own: everything it shows and does goes through the daemon's API, so the CLI and the web UI see the same thing ([ADR 0027](../blueprint/docs/adr/0027-desktop-app.md)).

> **Status:** CI builds and tests the app on macOS and Linux, but it has not yet been through a clean-install test on a Mac. Until a release is notarized, macOS blocks the downloaded app: Control-click it and choose **Open** the first time.

## Install and start

Download `Icaro_<version>_macos_universal.zip` from the [releases](https://github.com/ggrocco/icaro/releases), unzip it, and move `Icaro.app` to `/Applications`. The bundle includes the `icaro` CLI, which the app uses to start the daemon.

When the app launches:

1. It attaches to the daemon if one is already running, whether the CLI, `icaro service install` or an earlier launch started it. It never starts a second daemon.
2. Otherwise it starts `icaro serve` in the background, as the CLI does. It looks for `icaro` in this order: `$ICARO_BIN`, the copy inside the bundle, `/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, then `PATH`.
3. It opens the web UI in a window, unless tray-only mode is on.

Quitting the app leaves the daemon running, because the daemon owns your runs and sessions. Stop the daemon with the CLI.

The app reads the same configuration as the CLI. It honors `--config` and `--home` (passed to `icaro-app`) and `ICARO_HOME`. With `daemon.url` set, it connects to that TCP daemon with `daemon.token` and never starts one. Its window then opens the UI's sign-in page, because a one-time sign-in link can only be minted over the local socket.

## The menu

- **Status:** whether the daemon answers. If it does not, the menu shows the error and **Start daemon**.
- **Open Ícaro:** the web UI, signed in with a one-time link, as `icaro ui` does.
- **Active runs:** running and queued runs. Each has **Open run** and **Cancel run**.
- **Recent failures:** the last failed runs, with their errors. Click one to open it.
- **Run workflow:** your active workflows. A workflow that needs no trigger fields starts with one click. One that needs trigger fields (marked `…`) opens in the UI so you can fill them in.
- **Previews:** each preview's status. Click a running one to open its URL in your browser.
- **Today's usage:** tokens and the estimated cost from `icaro usage`. It is marked `≥` when some usage has no price, and flagged when the day is over `usage.daily_limit_usd`. Click it for the **Usage** page.
- **Launch at login**, **Tray only (no window at launch)** and **Quit Ícaro**.

On macOS, the number next to the icon is the number of active runs. A `!` means the daemon is not reachable.

## Notifications

The app posts a notification when:

- **A run fails.** Failures from before the app started are not repeated. A run that failed while the daemon was down still counts, for example one the daemon marked failed when it restarted. More than three failures at once become a single notification.
- **The daemon stops answering**, after two polls in a row (about 6 seconds). It posts again when the daemon is back.

Clicking a run notification opens the run. Clicking the outage notification starts the daemon. macOS asks for permission the first time; you can change it later in **System Settings → Notifications**.

## Launch at login and tray-only mode

- **Launch at login** registers the app as a login item: SMAppService on macOS 13 and later, a LaunchAgent on older versions, and an XDG autostart entry on Linux. You can see and remove it in **System Settings → General → Login Items**.
- **Tray only** makes every launch open no window, including the one at login. It is saved in `~/.icaro/config/desktop.json`. `icaro-app --tray-only` turns it on for one launch.

These are separate from `icaro service install`, which starts the daemon itself at login with no app at all ([Start at login](README.md#start-at-login)). You can use either one or both. If both start at login, the app attaches to the daemon the service started.

## Troubleshooting

- `icaro-app --debug` logs every menu update and notification to stderr. On macOS, run it as `/Applications/Icaro.app/Contents/MacOS/icaro-app --debug`.
- If the daemon will not start, its log is `~/.icaro/logs/daemon.log`, and `icaro doctor` checks for leftovers.
- A second launch, from Finder, Spotlight or `open -a Icaro`, does not start another copy. It brings up the running app's window.

## Building it

The app is a separate Go module under [`desktop/`](../desktop/) and needs cgo. On macOS, `make app-macos` builds a universal `bin/Icaro.app`, signed ad hoc. On Linux, the same app builds with `make app` after installing `libgtk-4-dev libwebkitgtk-6.0-dev`. It shows a tray only on desktops that host StatusNotifier items. Linux releases ship the CLI and daemon only.
