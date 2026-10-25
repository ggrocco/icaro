# Security

This page summarizes what Ícaro protects against and what it does not. The canonical policies are in the [security model](../blueprint/docs/03-security.md). To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## What you can rely on

**Containers.** Every container Ícaro creates, for agent sessions and workflow steps, runs:

- non-root, with no capabilities and `no-new-privileges`;
- under a seccomp profile without `ptrace`, `bpf`, `keyctl`, `mount` or namespace calls;
- with a read-only root filesystem and a tmpfs `/tmp`;
- with pids, memory, CPU and open-file limits;
- without the Docker socket.

A workflow file cannot weaken any of this. Preview stacks run your own containers, so they go through the [compose lint](previews.md#the-compose-lint) instead.

**Network.** An agent's credential lives inside its container, so the **egress allowlist** is the main protection: each profile's containers sit on a network with no route out, and reach only the profile's allowlisted hosts through a proxy. See [Sessions: Egress allowlist](sessions.md#egress-allowlist).

**Mounts.** Only the session worktree, the managed profile home and an explicit allowlist (`mounts.allow`) are mounted. The Docker socket, system paths, `$HOME` as a whole and credential directories (`~/.claude`, `~/.ssh`, `~/.aws`, …) are always refused. See [Sessions: Mounts](sessions.md#mounts).

**Secrets.** The master key is kept in the macOS Keychain or the Linux Secret Service, or in an encrypted file that needs a password. Secrets are sealed in the database, never shown by any API, CLI or MCP tool after they are written, and redacted from logs and captured outputs. Integration credentials are **host-bound**: the action executor refuses to send one to a host outside its binding. See [Integrations: Credentials](integrations.md#credentials).

**Integrations.** Inbound webhooks are verified (signatures, replay protection, size caps, rate limits) and deduplicated, and every delivery is recorded. Outbound actions cannot reach loopback, private or link-local addresses unless you opt in per integration, and they never follow redirects. Installing a pack shows its complete permission summary first, which is complete because manifests run no code.

**Local access.** The daemon listens on a `0600` Unix socket in a `0700` directory. TCP, whether `--listen` or the `icaro ui` loopback listener, requires a hashed, scoped API token or a browser session. Both are plain HTTP and loopback-only by default: a non-loopback `--listen` needs `--allow-remote` (`daemon.allow_remote`); put a TLS-terminating reverse proxy in front of a loopback listener instead. Cookie-authenticated requests that change something also need a CSRF header. See [Web UI](ui.md).

**Agents over MCP.** No tool returns a secret value or deletes anything, and memory scope comes from the session, not from the agent's arguments.

## What it does not protect against

- **An allowlisted endpoint is a channel.** A prompt-injected agent can still use its credential against hosts its profile allows, for example to spend quota at the provider. Keep allowlists short.
- **Docker non-root is a weak boundary.** Container escapes are a kernel/runtime problem. Stronger isolation (microVMs, gVisor via `docker.strict_runtime`) is optional and not the default.
- **What you put in a workflow.** A step whose `command` interpolates untrusted text (a PR title, an issue body) can be shell-injected. Pass such values through `env` ([Workflows](workflows.md#keep-untrusted-text-out-of-the-shell)).
- **Preview overrides.** A compose finding you allow with `icaro preview allow` weakens that preview's isolation on purpose.

## Checking your installation

`icaro doctor` checks:

- the daemon lock, socket and PID;
- permissions on `~/.icaro`;
- that Docker is reachable;
- pending migrations;
- the master-key backend;
- orphaned sessions;
- pinned image digests;
- the preview network, blocked compose files and `.test` DNS;
- pack load errors and missing webhook secrets;
- memory expiry.

`icaro doctor --fix` repairs what can be repaired.
