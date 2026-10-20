# ADR 0012 — Egress proxy topology and profile home layers

**Status:** Accepted — implements [ADR 0005](0005-persistent-home-egress.md); records the outcome of spike S2 on Linux

## Context

ADR 0005 makes the egress allowlist the primary mitigation for a credential that lives inside the agent container, and requires a golden seed home plus a per-session writable layer. It does not say how either is built.

## Decision

**Egress.**

- Each profile gets an **internal Docker network** `icaro-egress-<profile>` (`--internal`: no route off the host). Agent containers join only that network, so a direct connection to any outside address fails.
- One **proxy container** per profile runs `icaro egress-proxy --allow <list>` on that network (alias `icaro-egress`) and on the default bridge. It speaks HTTP `CONNECT` and absolute-URI HTTP, allows a host only if it matches the profile's allowlist (exact name or `*.domain`), never inspects TLS, and logs denials. Agents get `HTTP(S)_PROXY=http://icaro-egress:3128`.
- The proxy container runs under the same baseline as every Ícaro container (non-root `65534`, read-only root, no capabilities). Its image is `docker.helper_image` when configured, otherwise — on Linux — an image the daemon builds with `docker import` from its own static binary. The proxy is recreated whenever the allowlist changes (a hash label).
- An empty allowlist creates the network and no proxy: the container has no network at all.

**Homes.**

- The golden seed lives in `~/.icaro/home/<profile>/seed`. `icaro login <profile>` is the only writer: it runs the agent's login with the seed mounted read-write, under the baseline and the profile's egress.
- On Linux each session gets a Docker `local` volume of `type=overlay` (lower = seed, upper/work = `~/.icaro/home/<profile>/sessions/<id>`). Spike S2 passes there: two concurrent sessions read the same login and write isolated layers; the seed is never written.
- Where the engine cannot overlay host paths (Docker Desktop's VM, or a failed mount), the manager falls back to a **per-session copy** of the seed — S2's documented fallback. `sessions.home_mode` (`auto | overlay | copy`) overrides the choice.
- Ícaro writes its own agent configuration (the memory MCP entry, the handoff preamble) into the **session layer only**, merging with what the seed holds.

## Consequences

- The acceptance test "a request to a non-allowlisted host fails" is enforced by topology, not by trusting the agent to honor proxy variables.
- A token refreshed by an agent lands in its session layer and is lost when the session is removed; re-running `icaro login` refreshes the seed. Writing refreshed tokens back to the seed would let one session corrupt every other's login, so it is deliberately not done.
- On macOS the helper image must come from `docker.helper_image` (the agent base image contains `icaro`), because the daemon's own binary is not a Linux executable.
