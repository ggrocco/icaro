# Security model

## Main threats

1. Prompt injection making an agent exfiltrate mounted files.
2. A malicious project using Compose to escalate privileges or mount sensitive paths.
3. A forged webhook triggering expensive or destructive workflows.
4. A Slack/email/DNS token exposed in a log or a world-readable file.
5. A duplicate process competing for Docker, the DB, or previews.

## Mandatory policies

- An allowlist of mountable directories. Never mount the entire `$HOME` by default; the "shared home" option must be an explicit profile and show its impact during setup.
- The preview Compose goes through its own lint: block `privileged`, `pid: host`, `network_mode: host`, mounts outside the allowed root, the Docker socket, and dangerous capabilities, unless overridden with user confirmation.
- **Credential broker, not raw mounts (ADR 0003).** The daemon holds secrets and hands agents scoped, short-lived access over the session socket. No bind mount of `~/.claude`/`~/.config/gh`/`~/.aws` into an agent container by default; a raw mount needs an explicit profile and a setup-time warning. This is the primary mitigation for threat #1.
- Secrets in the macOS keychain / Linux secret store when available; fallback to an encrypted `0600` file only after an explicit password.
- Webhooks: mandatory HMAC, replay protection, a maximum timestamp, a persisted delivery id.
- Logs: a secret redactor for known patterns and structured records; never log the full env.
- Egress: a per-profile allowlist — **mandatory for untrusted profiles**, optional otherwise. Combined with the credential broker, this bounds an injected agent to limited creds and limited network. The MVP documents that Docker non-root is a weak boundary and evaluates per-container microVMs (Apple Containerization / gVisor / Firecracker) in the Phase 0/1 spike.
- Every destructive action requires `--yes` or UI confirmation; non-interactive mode requires an explicit flag.

## Minimum doctor

| Check | Severity | Remediation |
|---|---|---|
| Docker missing/inactive | error | OS-specific instructions |
| orphaned socket/pid | warning/error | `icaro doctor --fix` removes after validation |
| insecure permissions on `~/.icaro` | error | fixes to 0700/0600 |
| unverified image | error | re-pull by known digest |
| proxy without a dedicated network | error | recreates the network |
| missing webhook secret | error when enabled | generates/requests a secret |
| dangerous compose | error | shows the blocked field |
