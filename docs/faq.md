# FAQ

**Is Ícaro a CI system?**
No. Workflows orchestrate agent sessions and small automation around them. `act` and Dagger serve general-purpose local CI better ([ADR 0002](../blueprint/docs/adr/0002-session-spine.md)).

**Does Ícaro use my `~/.claude` login?**
No. Each profile has its own home under `~/.icaro/home/<profile>`, seeded once with `icaro login <profile>`. Your real `~/.claude` and other credential directories are never mounted.

**The agent can't reach a site it needs.**
That is the egress allowlist working. Add the host to the profile's `egress` list in `~/.icaro/config/icaro.yaml` (exact name or `*.domain`) and restart the session. Keep the list short: it is the main boundary around the agent's credential.

**A second `icaro serve` exits with code 3.**
One daemon per user ([ADR 0001](../blueprint/docs/adr/0001-single-daemon.md)). The CLI starts it on demand, so you rarely need `serve` at all. If the daemon crashed, `icaro doctor --fix` removes the stale PID and socket.

**Where is my data?**
In `~/.icaro`, or `ICARO_HOME`. It holds the config and SQLite database (`config/`), logs, profile homes, session worktrees, the repository cache, integration packs and preview state. Postgres is supported with `database.driver: postgres`.

**My workflow step can't write `/workspace`.**
Steps run as non-root with no capabilities. Write inside `/workspace` or `/tmp`; the root filesystem is read-only.

**Why is my workflow rejected for a secret?**
A workflow that references `${{ secrets.NAME }}` validates only once that secret exists. Create it with `icaro secret set NAME`; the value is read from stdin.

**My webhook didn't start a run.**
`icaro deliveries list` and `icaro deliveries get ID` say why: the signature failed, it was a duplicate, no event matched, the `filter` excluded it, or the concurrency policy skipped it. `icaro deliveries redeliver ID` replays a stored delivery.

**Does it work on a laptop without a public hostname?**
Sessions, previews, memory, manual and cron workflows do. Inbound webhooks need a daemon the provider can reach; on a laptop, use a `cron` workflow whose first step checks the source.

**What about the macOS app and tray?**
The native app is deferred until the session spine is stable (MVP cut 7). Today the surfaces are the CLI and the web UI (`icaro ui`), and `icaro service install` starts the daemon at login.

**Is there a usage or cost limit?**
`icaro usage` estimates spend from local transcripts and can alert on a daily limit. It tracks cost; it does not limit compute. The concurrency cap and container limits bound resources.
