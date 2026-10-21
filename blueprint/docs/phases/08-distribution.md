# Phase 8 — Distribution and updates

**Objective:** installer, Brew, and secure auto-update.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Create install.sh that verifies architecture, checksum, and signature.
- [x] Create a Homebrew tap/formula (GoReleaser cask for `ggrocco/homebrew-tap`).
- [x] Create a release pipeline with SBOM, checksums, and signature ([`.goreleaser.yaml`](../../../.goreleaser.yaml), [`release.yml`](../../../.github/workflows/release.yml), [ADR 0021](../adr/0021-release-signing-and-updates.md)).
- [x] Implement updates with staging and rollback (`icaro update`, `icaro update --rollback`).
- [x] Document config upgrade/migration ([docs/upgrading.md](../../../docs/upgrading.md)).

## Acceptance criteria

- [ ] A fresh install works on macOS and supported Linux. *Verified on Linux against a local signed mirror and a GoReleaser snapshot; macOS needs a real host.*
- [x] An invalid checksum blocks the installation (`hack/installtest`, `internal/update` tests).
- [x] An update preserves config and DB (it never writes under the state root).
- [x] An update while a session is running re-attaches to its container instead of orphaning it ([ADR 0006](../adr/0006-session-persistence-recovery.md)). *`hack/e2e` runs `icaro update` from a signed local mirror while a session runs: the daemon restarts, and the session keeps its container, its worktree and its memory socket (Linux; macOS waits on a real host).*
- [x] Rollback restores the previous binary.
