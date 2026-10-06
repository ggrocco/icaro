# ADR 0005 — Credentials: persistent home + mandatory egress

**Status:** Accepted — supersedes the default in [ADR 0003](0003-credential-broker.md)

## Context

ADR 0003 made a credential **broker** the Phase-1 default: agents would request
scoped, short-lived access over the session socket and never see a secret file.
In practice the target agent CLIs do not support this. Claude Code, Codex, and
Copilot CLI authenticate by reading their own home directory (`~/.claude`,
etc.); none exposes a "give me a scoped token over a socket" interface. The
broker's Phase-1 acceptance criterion ("no `~/.claude` bind mount") is also
satisfiable by simply injecting a long-lived API key as an env var — which
leaves a usable secret inside the very process we are defending against prompt
injection. A true broker requires an auth-injecting reverse proxy per provider,
which is a larger build than the MVP warrants.

## Decision

Credentials live in a **persistent per-profile home**, bounded by a **mandatory
egress allowlist**.

- A **persistent per-profile home** is mounted into the agent container. The
  user logs in once; the resulting `~/.claude` (and equivalents) is **seeded**
  and reused across all workflow steps and the dev container.
- The home is a **golden seed layer** (holds credentials, read-mostly) plus a
  **per-session writable overlay** (runtime writes: session state, history).
  Login is shared; concurrent sessions never corrupt one `~/.claude`.
- The **egress allowlist is mandatory and locked to provider endpoints by
  default.** With the secret now inside the container, this allowlist — not the
  withholding of the secret — is the primary mitigation for Threat #1. A raw
  bind mount of the host's real `~/.claude` is still forbidden; only the
  managed, seeded home is mounted.
- A **credential-injecting proxy** (`ANTHROPIC_BASE_URL` → daemon, real
  credential added host-side, container holds nothing usable) remains the
  desired end-state for **untrusted profiles** and is tracked as future
  hardening — not MVP-default.

## Consequences

- The acceptance test changes from "no `~/.claude` bind mount" to the
  discriminating one: **the egress allowlist confines every credential present
  in the container.** Mandatory egress moves from "optional otherwise" (security
  doc) to "on by default."
- **Residual risk, documented:** a credential in the container is usable for
  anything the allowlist permits (quota burn, calls to the provider); any
  allowlisted endpoint is a potential exfil channel. Bounded, not eliminated.
- The per-session overlay requires a Docker Desktop / Linux spike (see
  `docs/02-decisions-risks.md`, S2).
- ADR 0003 is downgraded: the broker/proxy is future hardening for untrusted
  profiles, not the MVP default.
