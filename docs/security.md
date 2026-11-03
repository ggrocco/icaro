# Security

This page summarizes what Ícaro protects against and what it does not. The canonical policies are in the [security model](../blueprint/docs/03-security.md). To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## What you can rely on

**Containers.** Every container Ícaro creates, for agent sessions and workflow steps, runs:

- non-root, with no capabilities and `no-new-privileges`;
- under a seccomp profile without `ptrace`, `bpf`, `keyctl`, `mount` or namespace calls;
- with a read-only root filesystem and a tmpfs `/tmp`;
- with pids, memory, CPU and open-file limits;
- without the Docker socket;
- on Linux, optionally under gVisor, a user-space kernel between the container and yours ([Sessions: Stronger isolation](sessions.md#stronger-isolation-gvisor)).

A workflow file cannot weaken any of this. Preview stacks run your own containers, so they go through the [compose lint](previews.md#the-compose-lint) instead.

**Network.** An agent's credential lives inside its container, so the **egress allowlist** is the main protection: each profile's containers sit on a network with no route out, and reach only the profile's allowlisted hosts through a proxy. See [Sessions: Egress allowlist](sessions.md#egress-allowlist).

**API keys never enter a container (opt-in).** A profile with `inject` keeps its Anthropic or OpenAI API key in the secret store, bound to the provider's host. The container gets only a per-session token. The daemon adds the key host-side, for the provider calls a coding session needs, and logs each request. A prompt-injected agent cannot copy the key out. See [Sessions: Credential injection](sessions.md#credential-injection-api-keys).

**Git credentials never enter a container.** `icaro session push` pushes a session's branch from the host with your credentials. Host git runs with hooks, fsmonitor, the file transport and submodule recursion disabled on session worktrees. See [Sessions: Pushing a session's branch](sessions.md#pushing-a-sessions-branch).

**Git you run yourself in a session worktree is not hardened.** The agent cannot change the project's `.git`, but it owns its worktree: a nested repository or submodule it creates there has its own config, which your git reads when you run it inside that directory or let it recurse into submodules. Review a session's changes with `git diff` from the project root (or the web UI), and push with `icaro session push` rather than running git inside directories the agent created.

**Mounts.** Only the session worktree, the managed profile home and an explicit allowlist (`mounts.allow`) are mounted. Container engine sockets and state directories, system paths, `$HOME` as a whole, credential directories (`~/.claude`, `~/.ssh`, `~/.aws`, …) and any directory that contains one (such as `~/.config`) are always refused. See [Sessions: Mounts](sessions.md#mounts).

**Secrets.** The master key is kept in the macOS Keychain or the Linux Secret Service, or in an encrypted file that needs a password. Secrets are sealed in the database, never shown by any API, CLI or MCP tool after they are written, and redacted from logs and captured outputs. Integration credentials are **host-bound**: the action executor refuses to send one to a host outside its binding. See [Integrations: Credentials](integrations.md#credentials).

**Integrations.** Inbound webhooks are verified (signatures, replay protection, size caps, rate limits) and deduplicated, and every delivery is recorded. Outbound actions cannot reach loopback, private or link-local addresses unless you opt in per integration, and they never follow redirects. Installing a pack shows its complete permission summary first, which is complete because manifests run no code.

**Local access.** The daemon listens on a `0600` Unix socket in a `0700` directory. TCP, whether `--listen` or the `icaro ui` loopback listener, requires a hashed, scoped API token or a browser session. Both are plain HTTP and loopback-only by default: a non-loopback `--listen` needs `--allow-remote` (`daemon.allow_remote`); put a TLS-terminating reverse proxy in front of a loopback listener instead. Cookie-authenticated requests that change something also need a CSRF header. See [Web UI](ui.md).

**Agents over MCP.** No tool returns a secret value or deletes anything, and memory scope comes from the session, not from the agent's arguments. No tool decides an [approval step](workflows.md#approvals-and-waits) either: a person does, with a `write` token or a UI session, and the decision records who. Containers reach only their session's memory endpoint, never the daemon API, so a run cannot approve itself.

## What it does not protect against

- **An allowlisted endpoint is a channel.** A prompt-injected agent can still use its credential against hosts its profile allows, for example to spend quota at the provider. Keep allowlists short.
- **An injected key can still be used.** With credential injection the agent cannot read the key, but it can still make the allowed provider calls through the proxy, so it can spend your quota. The per-session audit log shows which session did. Subscription logins (Claude Pro/Max, ChatGPT) are not injected; their token lives in the profile home.
- **Docker non-root is a weak boundary.** Container escapes are a kernel/runtime problem. On Linux, a profile with `runtime: runsc` runs under gVisor, so an escape also needs a gVisor bug; it is opt-in, and Docker Desktop on macOS has no gVisor runtime.
- **What you put in a workflow.** A step whose `command` interpolates untrusted text (a PR title, an issue body) can be shell-injected. Pass such values through `env` ([Workflows](workflows.md#keep-untrusted-text-out-of-the-shell)).
- **Preview overrides.** A compose finding you allow with `icaro preview allow` weakens that preview's isolation on purpose.

## Checking your installation

`icaro doctor` checks:

- the daemon lock, socket and PID;
- permissions on `~/.icaro`;
- that Docker is reachable;
- pending migrations;
- the master-key backend, and that no plaintext `master.key` from an early release is left in `~/.icaro/secrets` (an error; `icaro init` moves it into the OS key store or a password-protected file);
- that every `inject` secret exists and is bound to its provider's host;
- orphaned sessions;
- that every egress network (`icaro-egress-*`) is internal;
- that every profile's `runtime` is registered with Docker;
- pinned image digests;
- the preview network, blocked compose files and `.test` DNS;
- pack load errors and missing webhook secrets;
- memory expiry.

`icaro doctor --fix` repairs what can be repaired.
