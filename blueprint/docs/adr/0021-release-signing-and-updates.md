# ADR 0021 — Signed releases, staged updates, rollback

**Status:** Accepted

## Context

Phase 8 requires that installs and updates download only signed, checksummed artifacts, that an invalid checksum blocks installation, that an update preserves config and the database, that running sessions re-attach after an update, and that a rollback restores the previous binary. Verification has to work offline and without extra tools: neither `install.sh` nor `icaro update` can assume cosign is installed.

## Decision

- **One signed manifest per release.** GoReleaser publishes the archives, a `checksums.txt` (sha256), an SBOM per archive, and two signatures over `checksums.txt`:
  - `checksums.txt.sig`, a raw detached **ed25519** signature made by `hack/sign` with a key held only as a CI secret. This is the signature `install.sh` and `icaro update` verify.
  - `checksums.txt.sigstore.json`, a **cosign keyless** bundle for auditors (transparency log, workflow identity). Build provenance attestations cover the archives.
- **Pinned trust root.** The public key is compiled into the binary (`-X icaro/internal/update.DefaultPublicKey`) and embedded into the published `install.sh`. `update.public_key` can override it, for a mirror. With no key, both tools refuse the update; `install.sh --insecure-skip-signature` still verifies the checksum and prints a warning.
- **install.sh** verifies the signature with `openssl pkeyutl -rawin` (OpenSSL 3.0+), then the archive checksum, then installs atomically into `~/.local/bin`. It refuses root unless `--bin-dir` is given.
- **icaro update** downloads the archive, verifies it, and stages the new binary as `icaro.new`. It smoke-tests the staged binary with `icaro.new version`, keeps the current binary as `icaro.prev` (hard link, else copy), and renames the new one over it, which is atomic on one filesystem. `--rollback` renames `icaro.prev` back. Replacing the binary requires `--yes`.
- **Daemon handoff.** After a swap or rollback, a running daemon gets SIGTERM; once its lock is released, `icaro serve --background` starts the new binary. The startup reconciliation pass ([ADR 0006](0006-session-persistence-recovery.md)) re-attaches to session containers, which keep running across the restart. Updates never write under the state root.
- **Homebrew** ships through a cask in `ggrocco/homebrew-tap`. Until the binaries are notarized, the cask removes the quarantine attribute after install.

## Consequences

- An attacker needs the CI signing key to ship a binary; a compromised mirror or download URL is not enough.
- Rotating the key means shipping one release signed with the old key whose binary embeds the new key.
- Only one previous binary is kept. Rolling back across a schema migration also needs a database backup ([docs/upgrading.md](../../../docs/upgrading.md)).
- macOS code signing and notarization still need an Apple Developer identity. They are a Phase 7 release-pipeline item, and the cask's quarantine hook goes away once they land.
