# Security & operational model

## Main threats

1. Prompt injection making an agent exfiltrate mounted files or the credentials in its profile home.
2. A malicious project using Compose to escalate privileges or mount sensitive paths.
3. A forged webhook triggering expensive or destructive workflows.
4. A Slack/email/DNS token exposed in a log or a world-readable file.
5. A duplicate process competing for Docker, the DB, or previews.
6. A malicious integration pack templating a stored credential into an attacker-controlled URL, or an action URL targeting the daemon/LAN (SSRF).

## Mandatory policies

**Containers and mounts**

- **Container baseline** for the containers Ícaro creates — agent session containers and workflow step containers — applied with zero configuration:
  - non-root;
  - no `--privileged`, no Docker socket, no host network;
  - `--cap-drop ALL` and `no-new-privileges`;
  - a seccomp profile equal to Docker's default minus `ptrace`, `bpf`, `keyctl`, `mount`/`umount2`, `unshare`/`setns`, and the module syscalls;
  - a read-only root filesystem with tmpfs `/tmp`. Home is tmpfs too, except in agent containers, which mount the profile home;
  - pids, memory, CPU, and open-file limits.

  Nothing in a workflow file can weaken the baseline. Preview compose stacks run the user's own containers, so they are governed by the compose lint below instead.
- **Dangerous mounts in workflow files are rejected at validation time**, not by convention: the Docker socket and anything under `/var/run`, `/proc`, or `/sys`.
- **The daemon's own Docker access is narrowed** by a socket proxy that allows only the API calls the daemon makes, so a compromised daemon cannot `exec` into other containers or mount host paths.
- An allowlist of mountable directories. Never mount the entire `$HOME` by default; the "shared home" option must be an explicit profile and show its impact during setup (how it interacts with agent credential directories is [open item O4](02-decisions-risks.md#open-items)).
- The preview Compose goes through its own lint: block `privileged`, `pid: host`, `network_mode: host`, mounts outside the allowed root, the Docker socket, and dangerous capabilities, unless overridden with user confirmation.

**Agent credentials and egress** ([ADR 0005](adr/0005-persistent-home-egress.md))

- Agent credentials live only in the managed per-profile home, seeded at login. The host's real `~/.claude`/`~/.config/gh`/`~/.aws` is never bind-mounted.
- **Egress allowlist: mandatory, on by default, locked to provider endpoints.** With the credential inside the container, this allowlist is the primary mitigation for threat #1. The allowlist is defined per profile.
- Docker non-root is documented as a weak boundary; stronger isolation is spike S5 in the [feasibility gate](02-decisions-risks.md#feasibility-gate).

**Secrets and logs**

- Secrets in the macOS keychain / Linux secret store when available; fallback to an encrypted `0600` file only after an explicit password.
- Logs: a secret redactor for every known secret value (6+ characters), known patterns, and structured records, covering HTTP response bodies from integration actions; never log the full env.
- MCP surface (architecture §10): same auth as the API; no tool returns a secret value; no destructive tools in v1.

**Integrations** ([ADR 0007](adr/0007-integration-manifests.md))

- Webhooks: signature verification per the integration manifest (HMAC etc.), replay protection, a maximum timestamp, a persisted delivery id, body size caps, and rate limiting; unsigned generic webhooks require a per-workflow random URL token.
- **Integration credentials are host-bound.** Secrets used by `uses:` actions live in the daemon's secret store with a host allowlist the user confirms at setup. The HTTP executor refuses to attach a secret to a request whose host is outside that binding — a malicious pack update gets a refusal, not the token. These secrets never enter a container.
- **HTTP action executor:** private-range URLs (localhost, link-local, RFC 1918) blocked by default; self-hosted services need a per-integration user opt-in that a pack cannot grant itself.
- `icaro integration add` shows the pack's complete permission summary (hosts, auth kinds) before activation — complete because manifests execute no code.

**Operations**

- Every destructive action requires `--yes` or UI confirmation; non-interactive mode requires an explicit flag.

## `icaro doctor`

| Check | Severity | Remediation |
|---|---|---|
| Docker missing/inactive | error | OS-specific instructions |
| Orphaned lock/socket/PID (lock held by a non-existent PID, unreachable socket) | warning/error | `icaro doctor --fix` removes after validation |
| Orphaned sessions (container/worktree/preview gone after restart) | warning | re-attach or clean up the session ([ADR 0006](adr/0006-session-persistence-recovery.md)) |
| Insecure permissions on `~/.icaro` | error | fixes to 0700/0600 |
| Pending or failed migrations | error | runs or reports the migration |
| Unverified image | error | re-pull by known digest |
| Proxy without a dedicated network | error | recreates the network |
| Local DNS for `.test` misconfigured | warning when enabled | shows the resolver state; suggests `localhost:<port>` |
| Missing webhook secret | error when enabled | generates/requests a secret |
| Dangerous compose | error | shows the blocked field |
