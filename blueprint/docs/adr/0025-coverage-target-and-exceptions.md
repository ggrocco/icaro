# ADR 0025 — Coverage target and its exceptions

**Status:** Accepted

## Context

The [engineering standards](../05-engineering.md#coverage) set a 95%+ statement target for the domain `internal/*` packages and require every exclusion to carry a comment and an ADR. Phase 0 reported coverage in CI but did not enforce it: most packages sat at 70–90%, and the adapters at the edge (`cli`, `app`, `container/docker`) were at 0–27% in the unit run. There was no rule a reviewer could point to, and no list of what is allowed to be lower and why.

## Decision

- **The gate.** `hack/covercheck` reads the unit profile of the Linux CI job (`go test -race -coverprofile`) and fails any package under `icaro/internal/` below **95%**. Only packages listed in `hack/covercheck/thresholds.txt` may sit lower. Each line there names the package, a floor, and a reason. The macOS job does not run the gate, because platform files make its numbers differ.
- **Exceptions cannot go stale.** The gate also fails when a listed package reaches 95% (remove the line), when its floor is 5 or more points under real coverage (raise the floor), or when a listed package no longer exists. New packages get the 95% target automatically.
- **Every other package meets 95% today.** That covers the domain and the composition root `internal/app` (96%), which is tested against SQLite and a fake Docker API on a Unix socket.
- **The exceptions:**

  | Package | Floor | Measured | Why it is lower |
  |---|---|---|---|
  | `internal/cli` | 80% (unit) | 83% unit | The commands are tested against an in-process daemon. What is left starts real side effects: `docker exec`/`docker run` into a session (`sh`, `claude`, `login`, `attach`), `session create` waiting for Docker, `setup` pulling images, TTY input (`readSecret`, `passwordPrompt`), launching a browser, replacing the running binary (`update` apply and rollback), and installing launchd/systemd units. The integration TTY test and the e2e suites cover these; `internal/update` covers signing and the binary swap. |
  | `internal/container/docker` | 0% unit; **75% in the integration job** | 78–79% integration | The Docker SDK adapter. Faking the Docker API to unit-test it would mock exactly the behaviour the integration tests exist to check ([test strategy](../05-engineering.md#pyramid)). The integration job runs with `-coverpkg=./internal/container/docker` and gates the adapter at 75%. What stays uncovered there is pass-through error handling after SDK calls and `PullImage` from a registry. |

- **Unreachable statements inside packages that meet the target are not annotated.** Examples are `crypto/rand` failures, `json.Marshal` of types that cannot fail, `chmod` failing on a file the process owns, and the other OS's branch of a platform switch. The 5% margin exists for them, and annotating each would cost more than it tells a reader. Where such a branch was simply dead (since Go 1.24, `crypto/rand.Read` never returns an error), it was removed rather than tested.
- **Whole-suite measurement.** `make cover-e2e` builds an instrumented binary, runs the unit, integration and e2e suites with `GOCOVERDIR`, and writes a merged profile. It is a diagnostic for finding untested areas, not a gate.

## Consequences

- A change that drops a package below its target fails CI with the package and the number. Lowering an exception's floor, or adding one, needs this ADR updated, so it gets reviewed as a decision.
- The integration job now also measures coverage. `-coverpkg` adds a few seconds to that job.
- Coverage numbers are statements, not decisions. The [standards](../05-engineering.md#coverage) still ask for every business decision and critical path to be tested; the gate is the floor, not the goal.
- Writing the tests found four product bugs, each fixed with a regression test:
  - `creds`: preparing a session home returned a home that did not exist, with no error, when the seed could not be read for a reason other than "not exist".
  - `expr`: parse errors inside `${{ }}` pointed too early by the amount of leading whitespace.
  - `workflow`: trigger field checks depended on trigger order when one event was not in the catalog.
  - `cli`: `preview logs` missed previews whose path went through a symlink.
- One behaviour is recorded by a test but not changed here: the preview and integration handlers return internal errors (such as `sql: database is closed`) as 400 with the raw message, where other handlers return an opaque 500.
