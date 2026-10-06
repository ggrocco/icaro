# Phase 8 — Distribution and updates

**Objective:** installer, Brew, and secure auto-update.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Create install.sh that verifies architecture, checksum, and signature.
- [ ] Create a Homebrew tap/formula.
- [ ] Create a release pipeline with SBOM, checksums, and signature.
- [ ] Implement updates with staging and rollback.
- [ ] Document config upgrade/migration.

## Acceptance criteria

- [ ] A fresh install works on macOS and supported Linux.
- [ ] An invalid checksum blocks the installation.
- [ ] An update preserves config and DB.
- [ ] An update while a session is running re-attaches to its container instead of orphaning it ([ADR 0006](../adr/0006-session-persistence-recovery.md)).
- [ ] Rollback restores the previous binary.
