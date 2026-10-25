# Ícaro documentation

These pages describe how to use what is built. The design (architecture, decisions, security model, roadmap) lives in [`blueprint/`](../blueprint/README.md).

## Get started

- [Quickstart](quickstart.md): install, first session, first workflow, the web UI
- [Installing, upgrading and rolling back](upgrading.md)

## Use

- [Sessions and agents](sessions.md): worktrees, profiles, profile homes and `icaro login`, the egress allowlist, mounts, the concurrency cap
- [Workflows](workflows.md): the v1 format, steps, outputs, expressions, secrets, runs, concurrency, cron
- [Integrations](integrations.md): built-in integrations, credentials, webhooks and deliveries, actions, packs
- [Previews](previews.md): per-worktree preview URLs and the compose lint
- [Shared memory](memory.md): what agents remember and how they reach it
- [Agents and MCP](agents.md): `icaro mcp`, the authoring tools, `icaro skill install`
- [Web UI](ui.md): `icaro ui`, sign-in, what the UI shows
- [Desktop app](desktop.md): the menu bar app, notifications, launch at login
- [Usage tracking](#usage-tracking)
- [CLI reference](cli.md) (generated)

## Operate

- [Security](security.md): what you can rely on, what you cannot, and `icaro doctor`
- [Start at login](#start-at-login)
- [FAQ](faq.md)

## Usage tracking

`icaro usage` reads the Claude Code transcripts written in each profile home and prints tokens per day and model. With `--host-claude` it also reads your own `~/.claude`, read-only. Every row names its source and confidence. Costs are list-price estimates; models without a price entry show no cost, and no cost comes from the provider. Set `usage.daily_limit_usd` to get an alert when a day's estimated spend reaches it; days follow `usage.timezone`. An unreadable source shows as an `unavailable` row and never fails the command. The web UI's **Usage** page shows the same report for the profile homes (`GET /api/v1/usage?since=7d`); your own `~/.claude` stays a CLI opt-in.

## Start at login

`icaro service install` writes a launchd user agent (`~/Library/LaunchAgents/dev.icaro.daemon.plist`) on macOS or a systemd user unit (`~/.config/systemd/user/icaro.service`) on Linux, and starts it. The daemon runs headless; no window opens. `--dry-run` prints the unit, `icaro service status` reports it, and `icaro service uninstall --yes` removes it.
