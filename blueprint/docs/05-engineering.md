# Engineering standards

## Test strategy

### Pyramid

1. **Unit:** 80%+ of the volume. Business logic and validations, deterministic, with injectable clocks and executors; no real Docker.
2. **Integration:** real DB, real Docker API/CLI, and the daemon, in an ephemeral environment; no mocks of behavior that Docker already provides.
3. **E2E:** critical flows only (setup → container → workflow → preview → teardown), run in a separate CI job. Playwright for UI E2E.
4. **Security:** malicious Compose fixtures, webhook replay, forbidden mounts, and redaction.
5. **Regression:** every bug produces a test before the fix.

### Coverage

"Full coverage" means 100% of business decisions and critical paths covered, not a blind line-count metric. Technical target: 95%+ in the domain `internal/*` packages; any exclusion must have a comment and an ADR/issue.

### Surgical mocks

Allowed only at the edges: clock, filesystem, process runner, and external HTTP (integration actions included). Never mock the planner/executor itself to test the planner/executor.

### Tools

- `go test`, `-race`, fuzz on parsers/schema, `go vet`, staticcheck.
- Testcontainers or the Docker CLI in CI for integration; pick a single approach after a spike.

## Concurrency

Each goroutine owns its state and communicates over channels; no mutex in domain code. The store is a single writer goroutine fed by a command channel ([architecture §4](01-architecture.md#4-persistence)).

## Dependency policy

Prefer the standard library. Any dependency must have:

- active maintenance or proven stability;
- a compatible permissive license;
- a relevant community/contributor base;
- a small scope and a mature API;
- a pinned version and automated updates.

### Candidates to evaluate in a spike

- JSON Schema: `github.com/santhosh-tekuri/jsonschema/v6`.
- Cron parser (for the in-house scheduler): evaluate `robfig/cron/v3` versus a maintained alternative; abstract it behind an interface.
- Docker: CLI subprocess initially (more predictable for Compose); the Docker SDK only when it eliminates a concrete need.

Already decided (see the [decision index](02-decisions-risks.md#decision-index)): `modernc.org/sqlite` behind `database/sql` — CGO-based drivers such as `mattn/go-sqlite3` are avoided unless a concrete need arises — and React Flow, isolated in `web/`.

### Forbidden without an ADR

A heavy HTTP framework, an ORM, an event bus, a DI container, a mutex in the domain, a dependency on a specific agent, or a library that requires CGO without an explicit need.
