# Contributing to Ícaro

Thanks for helping. Ícaro is built against one design: [`blueprint/`](blueprint/README.md). Read the [roadmap](blueprint/docs/04-roadmap.md) and the phase file for the area you touch before writing code.

## Setup

- Go (the version in `go.mod`), Docker, and git.
- `make build` builds `bin/icaro`; `make test` runs the unit tests with `-race`.
- `make test-integration` also runs the tests tagged `integration` against a real Docker daemon.
- Set `ICARO_TEST_POSTGRES_DSN` to run the store tests against Postgres as well as SQLite:

  ```sh
  docker run -d -e POSTGRES_USER=icaro -e POSTGRES_PASSWORD=icaro -e POSTGRES_DB=icaro -p 5432:5432 postgres:18
  ICARO_TEST_POSTGRES_DSN='postgres://icaro:icaro@localhost:5432/icaro?sslmode=disable' make test
  ```

- `make lint` runs golangci-lint (gosec, staticcheck, errcheck, unused).
- `make upgrade` moves Go modules and npm packages to their latest minor/patch releases and regenerates the `THIRD_PARTY_LICENSES.md` files; major upgrades are done by hand. Wails, in the desktop app, is pinned to an exact beta and moves only by hand ([ADR 0027](blueprint/docs/adr/0027-desktop-app.md)).
- The desktop app is its own module under `desktop/` and needs cgo: the Xcode command line tools on macOS, `libgtk-4-dev libwebkitgtk-6.0-dev` on Linux. `make app-test` vets and tests it, `make app` builds `bin/icaro-app`, and `make app-macos` builds `bin/Icaro.app` on a Mac.
- `go run ./hack/linkcheck .` checks the relative links in every Markdown and HTML file.
- Parsers of untrusted input have fuzz targets (`FuzzParse` in `internal/expr`, `internal/workflow`, `internal/integration`; `FuzzParseCompose` in `internal/preview`). Their seeds run with the unit tests; CI fuzzes each for 20 seconds. Run one longer with `go test -run '^$' -fuzz '^FuzzParse$' -fuzztime 5m ./internal/workflow`, and commit any crasher it writes under `testdata/fuzz/` with the fix.

## Rules

The [engineering standards](blueprint/docs/05-engineering.md) are binding. In short:

- **Tests ship with the change.** Every bug gets a regression test before the fix. Mock only the edges: clock, filesystem, process runner, external HTTP.
- **Channels, not mutexes**, in domain code; the store is portable SQL with a migration per dialect (SQLite and Postgres).
- **Security boundaries don't move without review.** The container baseline, the egress allowlist, secret handling, and release verification are owned by [CODEOWNERS](.github/CODEOWNERS).
- **Decisions become ADRs.** Add one under [`blueprint/docs/adr/`](blueprint/docs/adr/) and a row to the [decision index](blueprint/docs/02-decisions-risks.md#decision-index).
- **No new dependency** without meeting the [dependency policy](blueprint/docs/05-engineering.md#dependency-policy); a heavy framework, ORM, event bus, or CGO library needs an ADR.
- Destructive commands require `--yes`.

## Pull requests

- Keep one concern per PR, and describe the user-visible effect and how you tested it.
- CI must be green: gofmt, vet, golangci-lint, unit tests on Linux and macOS, cross-builds, integration tests, the agent image build, the desktop app on Linux and macOS, and link checks.
- Tick the phase checkboxes your change completes.

## Releases

Maintainers tag a commit on `main` with `vX.Y.Z`; [`release.yml`](.github/workflows/release.yml) builds, signs, and publishes. See [ADR 0021](blueprint/docs/adr/0021-release-signing-and-updates.md) and [docs/upgrading.md](docs/upgrading.md).

### Cutting a release

```sh
git checkout main && git pull
make release TAG=v1.2.3 DRY_RUN=1   # only checks
make release TAG=v1.2.3             # tags origin/main and pushes the tag
```

[`hack/release.sh`](hack/release.sh) refuses a malformed tag, a dirty tree, a `HEAD` that is not the tip of `origin/main`, an existing tag, and a version older than the latest tag. A tag with a pre-release suffix (`v1.2.3-rc.1`) is published as a GitHub pre-release and does not update the Homebrew cask. Pushing a `v*` tag by hand (`git tag -a v1.2.3 -m v1.2.3 && git push origin v1.2.3`) starts the same workflow.

Once CI passes on the tagged commit, approve the `release` environment in the workflow run. The release then gets, for each of `linux_amd64`, `linux_arm64`, `darwin_amd64`, and `darwin_arm64`, a `icaro_<version>_<os>_<arch>.tar.gz` archive, plus `.deb` and `.rpm` packages for Linux, `Icaro_<version>_macos_universal.zip` (the desktop app), `install.sh`, and `checksums.txt` with its ed25519 and Sigstore signatures. Windows is not a target: the daemon depends on Unix sockets and `flock`.

### One-time setup

1. Generate the release key with `go run ./hack/sign keygen`. Keep the seed offline; losing it means users must reinstall to trust a new key.
2. In Settings → Environments, create the `release` environment (see below for its protection rules) and add the secret `ICARO_SIGNING_KEY` with the printed seed.
3. In Settings → Secrets and variables → Actions → Variables, add `ICARO_RELEASE_PUBKEY` with the printed public key. It is embedded in every binary and in `install.sh`.
4. Optional: add `HOMEBREW_TAP_TOKEN` to the `release` environment, a fine-grained token with contents write access to `ggrocco/homebrew-tap`. Without it the release still publishes, and the cask is not pushed.
5. Optional: the `MACOS_*` and `APPLE_*` secrets listed at the top of `release.yml`, to sign and notarize the desktop app. Without them it is signed ad hoc.
6. The `agent-image` job pushes to `ghcr.io/ggrocco/icaro-agent-base` (the default in `config.DefaultAgentImage`), in the namespace of this repository's owner, so the workflow's `GITHUB_TOKEN` creates the package on the first release. GHCR creates it private: after that first release, set its visibility to public (the package's settings page), or `icaro setup` cannot pull it. A fork sets the `AGENT_IMAGE` variable to an image it can push.

The `release` job fails first thing, before it builds anything, when the signing key or public key is missing.

### Gates

The release is gated:

1. `tag-on-main` refuses a tag whose commit is not on `main`.
2. `ci` runs the whole [`ci.yml`](.github/workflows/ci.yml) on the tagged commit: lint, race tests, the coverage gate, fuzzing, integration, e2e, the desktop app, and the agent image.
3. The jobs that publish (`release`, `macos-app`, `agent-image`) run in the `release` environment and wait for a reviewer's approval.

Every third-party action is pinned to a commit SHA with its version in a comment; Dependabot proposes the bumps. goreleaser, syft, and cosign run at exact versions.

A release is never the first run of its build. [`release-dry-run.yml`](.github/workflows/release-dry-run.yml) runs on every pull request that changes a release input (`.goreleaser.yaml`, `release.yml`, `install.sh`, `hack/sign`, `internal/update`): goreleaser in snapshot mode builds the archives, packages, cask and signed `install.sh`, and an ephemeral key signs and verifies `checksums.txt` the way the release does, with no secret and nothing published. CI's lint job also runs `actionlint` (with `shellcheck`) on every workflow, `release.yml` included. Locally: `goreleaser release --snapshot --clean --skip=sign,sbom,publish` with `ICARO_RELEASE_PUBKEY` and `HOMEBREW_TAP_TOKEN` set to any value.

These repository settings back the gates and must stay in place:

- **Environment `release`** (Settings → Environments): required reviewers (at least one maintainer other than the tagger when there is one), "Prevent self-review", and deployment tags limited to `v*`. It holds the secrets `ICARO_SIGNING_KEY`, `HOMEBREW_TAP_TOKEN`, `MACOS_CERTIFICATE`, `MACOS_CERTIFICATE_PASSWORD`, `APPLE_ID`, `APPLE_TEAM_ID`, and `APPLE_APP_PASSWORD`. Delete any repository-level copies of them, so no other workflow can read them.
- **Tag ruleset** for `v*` (Settings → Rules → Rulesets): only maintainers may create tags, and no one may update or delete them.
- **Branch protection on `main`**: pull requests with passing CI and code-owner review (see [CODEOWNERS](.github/CODEOWNERS)), no force pushes.
