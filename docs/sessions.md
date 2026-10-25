# Sessions and agents

A **session** is one unit of agent work: a git worktree, an isolated container running the agent against it, an optional live preview, and a scope in shared memory ([ADR 0002](../blueprint/docs/adr/0002-session-spine.md)). Run several at once, one per task.

```sh
cd ~/src/my-app
icaro claude                      # create-or-reuse this repository's session and attach claude
icaro codex                       # the same with codex
icaro sh                          # a shell in the session instead
icaro session create fix-login    # a new session on branch icaro/fix-login
icaro session list
icaro session attach ID           # reattach (-- CMD... runs a command instead)
icaro session stop ID             # stop the container; worktree and home are kept
icaro session start ID            # restart a stopped or orphaned session
icaro session rm ID --yes         # remove the session and its worktree (the branch is kept)
```

The agent gets a real TTY, and its exit code becomes `icaro`'s exit code.

## Worktrees

Ícaro is the only component that creates worktrees. `icaro session create [NAME]` runs `git worktree add -b icaro/NAME` under `~/.icaro/worktrees/<project>/<NAME>`, starting from `HEAD` (or `--base`). Without a name you get a short random one. Creating a session needs Docker to be reachable: otherwise `session create` (and the implicit create of `icaro sh`, `claude` and `codex`) fails at once with Docker's error, and nothing is created. Every worktree of a repository belongs to one **project**, whose id is derived from the main repository's path, so sessions on different branches of the same repository share project memory. The worktree is bind-mounted at `/workspace`, not copied, so your editor, the agent and the preview all see the same files. On Linux the container runs as your uid:gid (as `1000:1000` when the daemon runs as root), so files the agent writes are owned by you.

## Profiles

A profile picks the agent, the image, the egress allowlist and any extra mounts. `icaro setup` writes the built-in profiles to `~/.icaro/config/icaro.yaml`:

```yaml
profiles:
  claude:
    agent: claude
    image: ghcr.io/icaro-dev/agent-base:latest
    egress: [api.anthropic.com, console.anthropic.com, claude.ai, statsig.anthropic.com, sentry.io]
  codex:
    agent: codex
    image: ghcr.io/icaro-dev/agent-base:latest
    egress: [api.openai.com, auth.openai.com, chatgpt.com]
  shell:
    image: ghcr.io/icaro-dev/agent-base:latest
    egress: []
  workflow:                       # used by workflow image steps (workflows.profile)
    image: ghcr.io/icaro-dev/agent-base:latest
    egress: []
sessions:
  default_profile: claude
```

`--profile NAME` on `session create`, `sh`, `claude` and `codex` picks another profile. `icaro setup` pulls each image and pins it by digest; `icaro doctor` reports a pinned image that is missing or changed, and `--fix` pulls it again by its digest. The agent base image is built from [`images/agent-base/Dockerfile`](../images/agent-base/Dockerfile) (`make image-agent`).

## Profile homes and `icaro login`

Agent CLIs authenticate from their own home directory, so each profile has a persistent home under `~/.icaro/home/<profile>` ([ADR 0005](../blueprint/docs/adr/0005-persistent-home-egress.md)):

- `icaro login <profile>` runs the agent's login once, with the profile's **seed** home mounted read-write. It is the only command that writes to the seed.
- Every session mounts the seed **read-only** and gets its own writable layer on top (a Docker overlay volume on Linux, a per-session copy elsewhere; `sessions.home_mode: auto | overlay | copy`). Concurrent sessions share the login and never corrupt each other's `~/.claude`.
- A token an agent refreshes lands in that session's layer. Run `icaro login` again to refresh the seed. Because the seed is the lower layer of every running overlay session, the daemon refuses a login while sessions of the profile are queued, starting, or running on overlay homes, and names them; stop them first (`icaro session stop ID`), or pass `--force`.
- Your own `~/.claude`, `~/.codex`, `~/.ssh`, `~/.aws` and similar directories are never mounted.

Ícaro writes two things into each session's layer: the memory MCP server entry (`icaro mcp --socket /icaro/mcp.sock`) and the handoff preamble from `memory.preamble`. See [Shared memory](memory.md).

## Egress allowlist

Each profile's container joins an internal Docker network (`icaro-egress-<profile>`) with no route off the host. Its only way out is a per-profile proxy that allows the profile's `egress` hosts (exact names or `*.domain`) and denies everything else ([ADR 0012](../blueprint/docs/adr/0012-egress-proxy-and-home-layers.md)). An empty list (`egress: []`) means no network at all. The proxy enforces this by network topology, not by trusting the agent to honor proxy variables. If a network with that name already exists but is not internal, or was not created by Ícaro, the session refuses to start rather than reuse it; `icaro doctor` reports such networks and `--fix` removes the unused ones. Because the agent's credential lives inside its container, this allowlist is the main protection against a prompt-injected agent sending data elsewhere. Keep it short.

On macOS, set `docker.helper_image` (for example to the agent base image), because the daemon cannot build the Linux proxy image from a macOS binary.

## Mounts

The session manager adds its own mounts: the worktree at `/workspace`, the profile home, the session's `/icaro` directory, and the repository's `.git`. Inside `.git`, everything your git reads to decide what to run is read-only: `config`, `hooks/` (created empty if missing), submodule config and hooks, other worktrees' admin files, and the session worktree's `.git` link files, so an agent cannot plant a hook or setting that your git would later run. Objects, refs and the session's own index stay writable, so the agent can commit. You can add more with `profiles.<name>.mounts` (`host:container[:ro]`), but only under directories listed in `mounts.allow`:

```yaml
mounts:
  allow: [~/datasets]
profiles:
  claude:
    mounts: ["~/datasets/fixtures:/data:ro"]
```

Mount sources are resolved through symlinks before they are checked. These are refused even if allowed ([ADR 0011](../blueprint/docs/adr/0011-mount-policy.md)): the host root, `/var/run`, `/proc`, `/sys`, `/dev`, `/etc`, any ancestor of `$HOME`, any credential directory (`~/.ssh`, `~/.aws`, `~/.config/gh`, …), any directory that contains one (so `~/.config`, `~/.local` and `~/Library` are refused, while `~/.config/nvim` is fine), container engine state directories (`~/.colima`, `~/.orbstack`, `~/.rd`, `~/.lima`, …), and a container engine socket or a directory holding one near its top. A refused mount fails the session and gives the reason.

## Concurrency cap

At most `sessions.max_active` agent sessions run at once. The default, `0`, means one per 4 GiB of RAM, with a minimum of 1. Sessions over the cap are stored as `queued` and start when a slot frees. Each container is also bounded by `limits` (memory, CPUs, pids, open files).

## Restarts and recovery

Sessions are stored in the database. When the daemon starts, it probes every session that was not finished: one whose container still runs is re-attached, and one whose container or worktree is gone is marked `orphaned` ([ADR 0006](../blueprint/docs/adr/0006-session-persistence-recovery.md)). `icaro doctor` lists orphaned sessions. `icaro session start ID` restarts one, and `icaro doctor --fix` cleans them up while keeping their worktrees.

## Run sessions

Each workflow run also gets a session of kind `run`, whose workspace its steps share ([ADR 0010](../blueprint/docs/adr/0010-run-sessions.md)). `icaro session list --kind run` shows them.
