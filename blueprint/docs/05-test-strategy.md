# Test strategy

## Pyramid

1. Unit: 80%+ of the volume. Deterministic, with injectable clocks and executors.
2. Integration: real DB, real Docker API/CLI in fixtures; no mocks of behavior that Docker already provides.
3. E2E: setup → container → workflow → preview → teardown.
4. Security: malicious Compose fixtures, webhook replay, forbidden mounts, and redaction.

## Full coverage: operational interpretation

"Full coverage" should mean 100% of business decisions and critical paths covered, not a blind line-count metric. Technical target: 95%+ in the domain `internal/*` packages; any exclusion must have a comment and an ADR/issue.

## Surgical mocks

Allowed only at the edges: clock, filesystem, process runner, external HTTP, and notification provider. Never mock the planner/executor itself to test the planner/executor.

## Tools

- `go test`, `-race`, fuzz on parsers/schema, `go vet`, staticcheck.
- Testcontainers or the Docker CLI in CI for integration; pick a single approach after a spike.
- Playwright for UI E2E.
