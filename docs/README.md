# Ícaro documentation

User documentation. The design lives in [`blueprint/`](../blueprint/README.md); these pages describe how to use what is built.

## Get started

- Install — `install.sh`, Homebrew, or a release archive (see [Upgrading](upgrading.md) for verification and updates)
- Quickstart — *to be written against the final CLI*

## Use

- Sessions and agents — *to be written*
- Workflows — *to be written*
- Previews — *to be written*
- Shared memory — *to be written*
- Integrations — *to be written*
- Usage tracking — `icaro usage` (see [Usage tracking](#usage-tracking) below)

## Operate

- [Upgrading and rollback](upgrading.md)
- Start at login — `icaro service install` (see [Start at login](#start-at-login) below)
- Security — [SECURITY.md](../SECURITY.md) and the [security model](../blueprint/docs/03-security.md)
- FAQ — *to be written*

## Usage tracking

`icaro usage` reads the Claude Code transcripts written in each profile home (and, with `--host-claude`, your own `~/.claude`, read-only) and prints tokens per day and model. Every row names its source and confidence. Costs are list-price estimates and are left blank for models without a price entry; they never come from the provider. Set `usage.daily_limit_usd` to get an alert when a day's estimated spend reaches it; days follow `usage.timezone`. An unreadable source shows as an `unavailable` row and never fails the command.

## Start at login

`icaro service install` writes a launchd user agent (`~/Library/LaunchAgents/dev.icaro.daemon.plist`) on macOS or a systemd user unit (`~/.config/systemd/user/icaro.service`) on Linux, and starts it. The daemon runs headless; no window opens. `--dry-run` prints the unit, `icaro service status` reports it, and `icaro service uninstall --yes` removes it.
