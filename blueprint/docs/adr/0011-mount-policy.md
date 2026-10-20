# ADR 0011 — Mount policy: credential directories are never mounted

**Status:** Accepted — resolves [open item O4](../02-decisions-risks.md#open-items)

## Context

The mount policy allows an explicit "shared home" profile, while [ADR 0005](0005-persistent-home-egress.md) forbids mounting the host's real `~/.claude`. O4 asked whether a shared-home profile excludes credential directories or allows a raw credential mount behind a warning. Sessions also need the repository's `.git` directory, because a git worktree's `.git` file points at it by absolute path.

## Decision

- **Ícaro-managed mounts** are built by the session manager, not by user policy: the session worktree at `/workspace`, the profile home at `/home/icaro`, the session's `/icaro` directory (MCP socket), and the project's `.git` at its host path. Inside that `.git`, `hooks/` and `config` are mounted **read-only**, so an agent cannot plant a hook or `core.fsmonitor`/`core.hooksPath` setting that the host's git would later run.
- **User mounts** (`profiles.<name>.mounts`, `host:container[:ro]`) must resolve, after following symlinks, to a path under `mounts.allow`. A path outside the allowlist is not mounted, and the session fails with the reason.
- These are refused regardless of the allowlist: the host root, the Docker socket, `/var/run`, `/run`, `/proc`, `/sys`, `/dev`, `/etc`, `/boot`, any ancestor of `$HOME`, and any **credential directory** or anything below one (`~/.claude`, `~/.claude.json`, `~/.codex`, `~/.config/gh`, `~/.config/gcloud`, `~/.aws`, `~/.azure`, `~/.ssh`, `~/.gnupg`, `~/.docker`, `~/.kube`, `~/.netrc`, `~/.npmrc`, `~/.git-credentials`, `~/.icaro`, keyrings).
- `$HOME` itself is mountable only with an explicit shared-home profile (`MountPolicy.SharedHome`). Even then, every credential directory is covered by an empty tmpfs inside the container. **A raw credential mount is never allowed**; agents authenticate only from their profile home.

## Consequences

- O4 closes without a setup-time "are you sure" path, which users learn to click through.
- A symlink inside an allowed directory cannot smuggle a credential directory in, because the policy resolves sources before checking.
- An agent can still commit to the repository (objects and refs are writable), which is the point of a session; it cannot change what the host's git executes.
