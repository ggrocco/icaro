# Phase 5 — Previews per worktree

**Objective:** watcher, Compose, proxy, and cleanup ([architecture §7](../01-architecture.md#7-previews)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Create a worktree scanner/watch with debounce.
- [ ] Implement the [Compose lint](../03-security.md#mandatory-policies) and a safe policy.
- [ ] Create Traefik label overrides and a dedicated network.
- [ ] Bring compose up/down per worktree and record state; restart only on configuration change (compose/env/lockfile).
- [ ] Resolve optional `.test` and a later DNS-01 domain/TLS.
- [ ] Wire the **container↔preview seam** through `session`: the agent's `/workspace` and the preview's build context resolve to the same worktree, with polling watch configured on macOS.

## Acceptance criteria

- [ ] Creating a worktree with a compose file brings up a preview.
- [ ] Removing a worktree runs an idempotent cleanup.
- [ ] Two worktrees with the same slug do not collide.
- [ ] A dangerous compose file is blocked and explains why.
- [ ] An agent editing code inside its container produces a visible change in the preview (compose project with a declared port) on both macOS and Linux.
