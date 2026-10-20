# ADR 0024 — Hardening: performance budget, profiling, dependency gate

**Status:** Accepted

## Context

Phase 11 asks for load and chaos tests, a memory/CPU budget, profiling, and a dependency/license audit before GA. Each needs a number or a rule that CI can hold the code to, not a one-off measurement.

## Decision

- **Reference budget.** The numbers below were measured on the reference machine (Linux amd64 container, Docker 29.9, Go 1.26) and are asserted, with headroom, by the load tests. A regression past the asserted bound fails CI.

  | Measure | Measured | Asserted bound |
  |---|---|---|
  | Engine throughput: 500 runs × 5-step DAG, concurrency 8, fake Docker | 2.2 s (~1,100 steps/s) | completes in 5 min; every step exactly once |
  | Allocation per step (engine, fake Docker) | ~117 KiB | retained heap ≤ 256 MiB at full size |
  | SSE fan-out: 200 streams over 50 runs | connect 0.18 s; all `end` 0.09 s after runs finish | exactly one snapshot and one `end` per stream; ends within 60 s |
  | Goroutines after load | back to baseline (±5) | baseline + 5 |
  | Idle daemon | 62 MiB RSS, ~0.1 % CPU, 21 goroutines, 36 MiB heap | recorded, not asserted |
  | Memory search, 10k documents | p95 34 ms ([ADR 0020](0020-portable-memory-search.md)) | p95 < 50 ms |

  The small sizes run in every `go test -race`; `-tags load` runs the full sizes in CI.
- **Profiling stays local.** `icaro serve --pprof` (or `daemon.pprof: true`) mounts `net/http/pprof` under `/debug/pprof` on the **Unix socket only**. TCP listeners, including the UI listener, never serve it, because profiles expose memory contents.
- **Chaos scenarios are tests.** A daemon SIGKILLed mid-run re-attaches to a still-running step container; a step container lost while the daemon was down fails that attempt deterministically; a Docker outage fails attempts with the engine's error and retries carry steps across short outages; SQLite locked by another process is waited out up to `busy_timeout` and otherwise fails without partial rows or corruption.
- **Run completion order.** The engine finishes the run session (workspace cleanup) before the run turns terminal, so a terminal run never has a live session behind it. The flake hunt found the opposite order.
- **Dependency gate.** `hack/licenses` audits the Go modules linked into the binary and the UI's npm production dependencies. MIT, BSD, Apache-2.0, ISC, MPL-2.0, Unlicense, 0BSD, CC0 and BlueOak are allowed; GPL, AGPL, LGPL and unknown licenses fail. The generated `THIRD_PARTY_LICENSES.md` files must be current. `govulncheck` runs in CI against the current stable Go patch release.

## Consequences

- Performance and leak regressions show up as failing tests, not as anecdotes.
- Profiling needs local access to the socket, so it is safe to leave the flag on during an investigation.
- A new dependency with a missing or copyleft license blocks the merge until it is replaced or the policy changes in a new ADR.
- Release builds should use the current Go patch release; govulncheck checks against it, not against the `go.mod` minimum.
