# Installing, upgrading and rolling back

## Install

```sh
curl -fsSL https://github.com/ggrocco/icaro/releases/latest/download/install.sh | sh
```

`install.sh` detects your OS (Linux or macOS) and architecture (amd64 or arm64), then downloads the release archive, `checksums.txt` and `checksums.txt.sig`. It verifies the ed25519 signature over `checksums.txt` with the public key embedded in the published script (this needs OpenSSL 3.0+), then checks the archive's sha256 against that list. If either check fails, it installs nothing. By default it installs into `~/.local/bin`. It refuses to run as root unless you pass `--bin-dir`. Options:

| Option | Effect |
|---|---|
| `--version v0.3.0` | install a specific release |
| `--bin-dir DIR` | install somewhere else (required as root) |
| `--base-url URL` | use a mirror laid out as `URL/latest` and `URL/<tag>/<asset>` |
| `--insecure-skip-signature` | skip the signature check (the checksum is still verified); prints a warning |

On macOS you can also use Homebrew: `brew install ggrocco/tap/icaro`.

## Upgrade

```sh
icaro update --check     # is there a newer release?
icaro update --yes       # install it
```

`icaro update` applies the same checks as `install.sh`, verifying the signature against the key compiled into the binary (or `update.public_key` in the config). It writes the new binary next to the current one as `icaro.new` and runs `icaro.new version` as a smoke test. Then it renames the new binary over the old one, which is atomic, and keeps the previous binary as `icaro.prev`. If a daemon is running, `icaro update` stops it with SIGTERM and starts the new binary with `icaro serve --background`. Running sessions re-attach to their containers through the startup reconciliation pass ([ADR 0006](../blueprint/docs/adr/0006-session-persistence-recovery.md)). Pass `--no-restart` to restart the daemon yourself.

An update never touches the state root (`~/.icaro`): the config, the database, profile homes, and worktrees stay where they are.

Set `update.base_url` to update from a mirror rather than GitHub.

## Roll back

```sh
icaro update --rollback --yes
```

This restores `icaro.prev` and restarts the daemon. Only one previous binary is kept.

## Config and database migrations

- **Config.** `~/.icaro/config/icaro.yaml` has a `version` key (currently `1`). A binary refuses a config written for a newer schema version instead of half-applying it. When a release bumps the config version, its notes describe the change, and the old version keeps loading until you edit the file.
- **Database.** Schema changes are numbered migrations for SQLite and Postgres. The daemon applies pending migrations at startup. `icaro migrate` applies them by hand, and `icaro doctor` reports pending ones (`icaro doctor --fix` applies them).
- **Rolling back across a migration.** An older binary does not undo migrations a newer one applied. Before upgrading across a release whose notes list a migration, stop the daemon and back up `~/.icaro/config/icaro.db` (a plain file copy); to roll back, restore that copy along with `icaro.prev`.

## Start at login

`icaro service install` keeps the daemon running across logins and crashes. See [Start at login](README.md#start-at-login).
