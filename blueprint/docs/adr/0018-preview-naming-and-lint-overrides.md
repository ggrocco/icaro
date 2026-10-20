# ADR 0018 — Preview hostnames default to `*.localhost`; lint overrides are per finding

**Status:** Accepted

## Context

Architecture §7 asks for a human-readable `slug.test` hostname "only when it does not collide", a predictable `http://localhost:<port>` alternative, and local `.test` resolution only with consent (spike S7). The compose lint in 03-security blocks dangerous fields "unless overridden with user confirmation", but does not say what is confirmed or where that is recorded.

## Decision

- **Naming.** A preview's identity derives from its absolute worktree path alone: slug = sanitized base name (at most 30 chars), hash = 8 hex chars of SHA-256(path), compose project = `icaro-<slug>-<hash>`. Cleanup can recompute it after the worktree and its database row are gone.
- **Hostnames.** `<slug>.<domain>`. If another worktree already holds it, the name becomes `<slug>-<hash>.<domain>`. The choice is made on the manager's single goroutine and stored with a unique index, so two worktrees with the same slug never collide.
- **Default domain is `localhost`.** Browsers and most resolvers send `*.localhost` to loopback (RFC 6761), so `http://<slug>.localhost:<previews.http_port>` works with no DNS setup. `previews.domain: test` opts into `.test`, which needs a local resolver the user configures; `icaro doctor` warns when `icaro-probe.test` does not resolve to loopback, and never edits the resolver. Every preview also prints the always-working form `curl -H 'Host: <host>' http://127.0.0.1:<port>/`. DNS-01 TLS for a custom domain stays deferred.
- **Lint overrides are per finding.** Each finding has a stable ID such as `web/privileged`, `web/volumes/0` or `web/cap_add/SYS_ADMIN`. `icaro preview allow <path> --finding <id> --yes` (admin scope; the API requires `confirm: true`) records that one ID for that one worktree, with the actor, in `preview_allowances`. A new dangerous field is still blocked, and removing the preview clears its overrides.
- **Configuration-change restarts.** A preview restarts only when the hash of its compose files, `.env`, `Dockerfile` and known lockfiles changes, or when its stack is not running. Code edits reach the dev server through the bind mount; hot reload is the app's job.
- **Watcher.** The session worktrees root and `previews.roots` are polled every `previews.debounce`. A change is applied only once two consecutive scans agree. A preview the user stopped with `icaro preview down` is left alone by the watcher until an explicit `up`.

## Consequences

- Previews work out of the box on macOS and Linux without touching DNS, and `.test` stays an opt-in with a doctor check.
- An override can never quietly widen: allowing `web/privileged` does not allow a later Docker-socket mount.
- Hostnames are stable across daemon restarts, because they are persisted, not recomputed.
