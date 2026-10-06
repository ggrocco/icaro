# ADR 0003 — Credentials are brokered, not mounted

**Status:** Superseded by [ADR 0005](0005-persistent-home-egress.md) — the broker is downgraded from MVP-default to future hardening for untrusted profiles. The MVP default is a persistent per-profile home bounded by a mandatory egress allowlist.

## Context

Threat #1 in the security model is prompt injection causing an agent to exfiltrate mounted files. Agent CLIs (Claude Code, Codex, Copilot, etc.) need provider credentials to function. The original plan — "selective read-only mounts, document the risk" — exposes long-lived tokens (e.g. a bind mount of `~/.claude`) to a process running semi-trusted model output with network access. That is the product's central security tension, and "document the risk" does not resolve it.

## Decision

Credentials are **brokered**, never raw-mounted into agent containers.

- The daemon holds credentials (OS keychain / secret store, encrypted `0600` fallback) and exposes a scoped broker over the per-session Unix socket. Agents request access; they never see the underlying secret file.
- Prefer **short-lived / scoped tokens** where the provider supports them (OAuth refresh handled host-side, minted access tokens passed in).
- Where a provider only supports a long-lived key, scope it **per profile** and document the residual risk explicitly; this is the documented-risk fallback, not the default.
- No bind mount of `~/.claude`, `~/.config/gh`, `~/.aws`, or similar into an agent container by default. Such a mount requires an explicit profile and a setup-time warning.

## Consequences

- A credential broker is a **Tier-1 feature**, designed alongside the container in Phase 1 — not deferred hardening. It is plausibly part of Ícaro's moat.
- The broker interface is provider-pluggable (`internal/integration/creds` or similar); each agent gets an adapter describing how it authenticates.
- Combined with the egress allowlist (see `docs/03-security.md`), this bounds the blast radius of an injected agent: limited creds, limited network.
