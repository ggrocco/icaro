# Phase 5 — Previews per worktree

**Objective:** watcher, Compose, proxy, and cleanup ([architecture §7](../01-architecture.md#7-previews)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Create a worktree scanner/watch with debounce.
- [x] Implement the [Compose lint](../03-security.md#mandatory-policies) and a safe policy.
- [x] Create Traefik label overrides and a dedicated network. *(As a network-alias override plus file-provider routes, [ADR 0017](../adr/0017-preview-proxy-file-provider.md).)*
- [x] Bring compose up/down per worktree and record state; restart only on configuration change (compose/env/lockfile).
- [ ] Resolve optional `.test` and a later DNS-01 domain/TLS. *(`*.localhost` default and `.test` opt-in with a doctor check are done ([ADR 0018](../adr/0018-preview-naming-and-lint-overrides.md)); DNS-01/TLS is still deferred.)*
- [x] Wire the **container↔preview seam** through `session`: the agent's `/workspace` and the preview's build context resolve to the same worktree, with polling watch configured on macOS. *(`preview.Manager` implements `session.PreviewHook`, and the daemon hooks it into the session manager in `internal/app/previews.go`.)*

## Acceptance criteria

- [x] Creating a worktree with a compose file brings up a preview.
- [x] Removing a worktree runs an idempotent cleanup.
- [x] Two worktrees with the same slug do not collide.
- [x] A dangerous compose file is blocked and explains why.
- [ ] An agent editing code inside its container produces a visible change in the preview (compose project with a declared port) on both macOS and Linux. *(Linux: verified, an edit made inside the agent container reaches the preview (`hack/e2e`). macOS: not yet run (spike S1).)*
