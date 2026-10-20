# Contributing to Ícaro

Thanks for helping. Ícaro is built against one design: [`blueprint/`](blueprint/README.md). Read the [roadmap](blueprint/docs/04-roadmap.md) and the phase file for the area you touch before writing code.

## Setup

- Go (the version in `go.mod`), Docker, and git.
- `make build` builds `bin/icaro`; `make test` runs the unit tests with `-race`.
- `make test-integration` also runs the tests tagged `integration` against a real Docker daemon.
- Set `ICARO_TEST_POSTGRES_DSN` to run the store tests against Postgres as well as SQLite:

  ```sh
  docker run -d -e POSTGRES_USER=icaro -e POSTGRES_PASSWORD=icaro -e POSTGRES_DB=icaro -p 5432:5432 postgres:16
  ICARO_TEST_POSTGRES_DSN='postgres://icaro:icaro@localhost:5432/icaro?sslmode=disable' make test
  ```

- `make lint` runs golangci-lint (gosec, staticcheck, errcheck, unused).
- `go run ./hack/linkcheck .` checks the relative links in every Markdown and HTML file.

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
- CI must be green: gofmt, vet, golangci-lint, unit tests on Linux and macOS, cross-builds, integration tests, and link checks.
- Tick the phase checkboxes your change completes.

## Releases

Maintainers tag `vX.Y.Z`; [`release.yml`](.github/workflows/release.yml) builds, signs, and publishes. See [ADR 0021](blueprint/docs/adr/0021-release-signing-and-updates.md) and [docs/upgrading.md](docs/upgrading.md).
