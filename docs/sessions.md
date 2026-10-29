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
icaro session push ID             # push its branch from the host with your git credentials
icaro session rm ID --yes         # remove the session and its worktree (the branch is kept)
```

The agent gets a real TTY, and its exit code becomes `icaro`'s exit code.

`session stop` and `session rm` also work on a session that is still `queued` or starting: the stop wins, and a start already under way removes the container it created instead of running it (a `session start` issued after the stop wins in turn, and `session rm` then refuses). If Docker cannot stop the container, the session is marked `error` with the reason (not `stopped`), and running `session stop` again retries.

## Worktrees

Ícaro is the only component that creates worktrees. `icaro session create [NAME]` runs `git worktree add -b icaro/NAME` under `~/.icaro/worktrees/<project>/<NAME>`, starting from `HEAD` (or `--base`). Without a name you get a short random one. Creating a session needs Docker to be reachable: otherwise `session create` (and the implicit create of `icaro sh`, `claude` and `codex`) fails at once with Docker's error, and nothing is created. Every worktree of a repository belongs to one **project**, whose id is derived from the main repository's path, so sessions on different branches of the same repository share project memory. The worktree is bind-mounted at `/workspace`, not copied, so your editor, the agent and the preview all see the same files. On Linux the container runs as your uid:gid (as `1000:1000` when the daemon runs as root), so files the agent writes are owned by you.

Every git command the daemon runs on a repository or a session worktree uses `core.fsmonitor=false`, `core.hooksPath=/dev/null`, `protocol.file.allow=never` and `submodule.recurse=false`, because the agent controlled the worktree's contents. Your hooks (for example `post-checkout`) therefore do not run when Ícaro creates a worktree, and no program a worktree names is run on the host.

### Pushing a session's branch

Agent containers hold no git credentials. Push from the host instead:

```sh
icaro session push ID                       # push icaro/NAME to origin
icaro session push ID --remote upstream     # another configured remote
icaro session push ID --force-with-lease    # after the agent rewrote history (asks first; --yes skips)
```

The daemon pushes the commit the session's branch points at to `refs/heads/icaro/NAME`, and nothing else: no tags (even with `push.followTags`) and no submodules. It runs from the main checkout with your git credentials, so the daemon's environment needs them (a credential helper or `ssh-agent`). `--remote` must name a remote configured in the repository. URLs, paths and mirror remotes are refused, and so is a remote whose URL is a local path, because the file transport is never used for session pushes. A plain force push is never done. `--force-with-lease` overwrites the remote branch only while it still points where your remote-tracking ref says (`git fetch` first). If there is no such ref, it requires that the branch does not exist on the remote yet. The agent can move that tracking ref in the shared `.git`, so the lease protects against clobbering work nobody fetched, not against the agent. Either way only the session's own branch can be overwritten. Your `pre-push` hook does not run.

## Profiles

A profile picks the agent, the image, the egress allowlist and any extra mounts. `icaro setup` writes the built-in profiles to `~/.icaro/config/icaro.yaml`:

```yaml
profiles:
  claude:
    agent: claude
    image: ghcr.io/icaro-dev/agent-base:latest
    egress: [api.anthropic.com, console.anthropic.com, claude.ai, statsig.anthropic.com]
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

The `claude` allowlist leaves out `sentry.io`, Claude Code's error reporting: Sentry is a shared ingest service anyone can send data to, so allowing it gives a prompt-injected agent a way out. Claude Code works without it. To send its crash reports anyway, add `sentry.io` to the profile's `egress` list. Configs written by an older `icaro setup` may still list it; remove it there to close that path.

`--profile NAME` on `session create`, `sh`, `claude` and `codex` picks another profile. `icaro setup` pulls each image and pins it by digest; `icaro doctor` reports a pinned image that is missing or changed, and `--fix` pulls it again by its digest. The agent base image is built from [`images/agent-base/Dockerfile`](../images/agent-base/Dockerfile) (`make image-agent`).

## Profile homes and `icaro login`

Agent CLIs authenticate from their own home directory, so each profile has a persistent home under `~/.icaro/home/<profile>` ([ADR 0005](../blueprint/docs/adr/0005-persistent-home-egress.md)):

- `icaro login <profile>` runs the agent's login once, with the profile's **seed** home mounted read-write. It is the only command that writes to the seed.
- Every session mounts the seed **read-only** and gets its own writable layer on top (a Docker overlay volume on Linux, a per-session copy elsewhere; `sessions.home_mode: auto | overlay | copy`). Concurrent sessions share the login and never corrupt each other's `~/.claude`.
- A token an agent refreshes lands in that session's layer. Run `icaro login` again to refresh the seed. Because the seed is the lower layer of every running overlay session, the daemon refuses a login while sessions of the profile are queued, starting, or running on overlay homes, and names them; stop them first (`icaro session stop ID`), or pass `--force`. While the login runs, new sessions of the profile wait in `queued` and start when it ends; a login whose CLI never reports back (it crashed) stops holding the profile after 30 minutes. Only one login per profile runs at a time.
- Your own `~/.claude`, `~/.codex`, `~/.ssh`, `~/.aws` and similar directories are never mounted.

Ícaro writes two things into each session's layer: the memory MCP server entry (`icaro mcp --socket /icaro/mcp.sock`) and the handoff preamble from `memory.preamble`. See [Shared memory](memory.md).

## Credential injection (API keys)

With a provider **API key**, a profile can keep the key out of every container ([ADR 0028](../blueprint/docs/adr/0028-credential-injection.md)). Store the key once, bound to the provider's API host:

```sh
icaro secret set anthropic-key --host api.anthropic.com   # the value is read from stdin or a hidden prompt
icaro secret set openai-key --host api.openai.com
```

Then point the profile at it:

```yaml
profiles:
  claude:
    inject:
      anthropic: anthropic-key
  codex:
    inject:
      openai: openai-key
```

Restart the daemon after changing `inject`. The agent container of such a profile gets `ANTHROPIC_BASE_URL=http://icaro-egress:3129/anthropic` (or `OPENAI_BASE_URL=http://icaro-egress:3129/openai/v1`) and, as its API key, a token that only identifies the session. Its requests go to the profile's egress proxy, which relays them over a socket that only the proxy mounts. The daemon then:

- checks that the token belongs to a running session of the profile;
- allows only the calls a coding session needs (Anthropic messages, token counting and models; OpenAI responses, chat completions and models);
- removes any credential the client sent and adds the real key;
- sends the request to the provider over verified TLS and streams the answer back.

Each request is logged, without bodies, to `~/.icaro/logs/inject/<session>.log`: method, path, status, bytes and duration. The token changes on every session start and stops working when the session stops or is removed. `icaro doctor` warns when an `inject` secret is missing or is not bound to the provider's host.

What this protects against, and what it does not:

- **The key cannot be copied out.** It is not in the container's environment, files, home or mounts, nor in the proxy container. A prompt-injected agent has nothing to exfiltrate.
- **The key can still be used.** A compromised agent can make the allowed provider calls through the proxy (spending your quota, for example), and the audit log tells you which session did. Because the agent no longer calls the provider directly, you can drop its API host (`api.anthropic.com`, `api.openai.com`) from the profile's `egress` list. Keep the other hosts the CLI needs.
- **Subscription logins are not covered.** With a Claude Pro/Max or ChatGPT sign-in (`icaro login`), the CLI keeps and refreshes its own token in the profile home, as described above. Profiles without `inject` work as before.
- **Not yet verified on real installs (spike S4):** whether the current Claude Code and Codex accept the placeholder key and base URL with no extra prompt, and whether they call endpoints outside the allowlist. A refused call shows up as a `403` from Ícaro and in the audit log.

## Egress allowlist

Each profile's container joins an internal Docker network (`icaro-egress-<profile>`) with no route off the host. Its only way out is a per-profile proxy that allows the profile's `egress` hosts (exact names or `*.domain`) and denies everything else ([ADR 0012](../blueprint/docs/adr/0012-egress-proxy-and-home-layers.md)). An empty list (`egress: []`) means no network at all. The proxy enforces this by network topology, not by trusting the agent to honor proxy variables. If a network with that name already exists but is not internal, or was not created by Ícaro, the session refuses to start rather than reuse it; `icaro doctor` reports such networks and `--fix` removes the unused ones. Because the agent's credential lives inside its container (unless the profile uses [credential injection](#credential-injection-api-keys)), this allowlist is the main protection against a prompt-injected agent sending data elsewhere. Keep it short.

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

## Stronger isolation (gVisor)

On Linux, a profile can run its containers under [gVisor](https://gvisor.dev), a user-space kernel between the container and yours, so a container escape needs a gVisor bug as well as a kernel bug ([ADR 0029](../blueprint/docs/adr/0029-gvisor-runtime.md)). Everything above still applies; gVisor is added underneath it.

1. [Install gVisor](https://gvisor.dev/docs/user_guide/install/), then register it with Docker so that containers may connect to host sockets mounted into them. The session's memory socket, `/icaro/mcp.sock`, is one; Ícaro mounts no other.

   ```sh
   sudo runsc install -- --host-uds=open
   sudo systemctl restart docker
   ```

2. Set the profile's runtime and restart the daemon:

   ```yaml
   profiles:
     claude:
       runtime: runsc
   ```

New sessions of the profile start under gVisor; a running session keeps its container until it is recreated. A workflow's image steps run on the runtime of `workflows.profile` (`workflow` by default). Ícaro's own helpers (the egress proxy, the preview proxy) and preview stacks stay on Docker's default runtime. `icaro doctor` reports a profile whose runtime Docker has not registered.

System calls are slower under gVisor, so builds and tests in a session take longer. gVisor's own kernel runs as threads inside the container's process limit, so keep `limits.pids` at its default (512) or higher: with 64, a session with many mounts failed to start. Docker Desktop on macOS has no gVisor runtime.

## Concurrency cap

At most `sessions.max_active` agent sessions run at once. The default, `0`, means one per 4 GiB of RAM, with a minimum of 1. Sessions over the cap are stored as `queued` and start when a slot frees. Each container is also bounded by `limits` (memory, CPUs, pids, open files).

## Restarts and recovery

Sessions are stored in the database. When the daemon starts, it probes every session that was not finished: one whose container still runs is re-attached, and one whose container or worktree is gone is marked `orphaned` ([ADR 0006](../blueprint/docs/adr/0006-session-persistence-recovery.md)). The daemon keeps probing running sessions while it runs and does the same. When a session is marked `orphaned` and its container is still up, the container is stopped (not removed), so no agent with your credentials keeps running unsupervised. A session's preview is optional and is re-synced by the preview manager itself: a missing preview never orphans a session, and the stale link to it is dropped. `icaro doctor` lists orphaned sessions. `icaro session start ID` restarts one, and `icaro doctor --fix` cleans them up while keeping their worktrees. `doctor --fix` works on the database and Docker directly instead of going through the daemon (a known divergence from [ADR 0001](../blueprint/docs/adr/0001-single-daemon.md)), so it claims each session as it goes and skips one that is no longer orphaned, such as one the daemon re-attached or restarted after doctor listed it.

## Run sessions

Each workflow run also gets a session of kind `run`, whose workspace its steps share ([ADR 0010](../blueprint/docs/adr/0010-run-sessions.md)). `icaro session list --kind run` shows them.
