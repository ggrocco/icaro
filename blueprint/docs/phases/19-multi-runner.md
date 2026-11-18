# Phase 19 — Multi-runner scale-out

**Objective:** workflow runs scale past one machine. One primary per Postgres database keeps everything it does today, and runner daemons on other machines claim and execute workflow runs from the shared database, with no inbound port, as proposed in [ADR 0040](../adr/0040-multi-runner-scale-out.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs ADR 0040 **accepted**, with its open questions settled:

- label-based placement in v1 or later;
- the `cluster.lease` default, and whether a lost node's runs fail or are re-queued;
- where runner logs go;
- which key runners hold.

A change to any of them is made in the ADR first, then here.

**Order.** The deliverables are built in the order listed. Each one keeps a single-node daemon on SQLite or Postgres behaving exactly as today, so the phase can merge step by step, and the existing suites are the guard.

## Deliverables

- [ ] **Owner-aware claims (single node first).**
  - The engine claims under a stable node id kept in `runner_meta`, instead of the constant `"daemon"`. Existing rows with `claimed_by = 'daemon'` are adopted by the node on its first start after the upgrade.
  - Startup recovery resumes only runs this node claimed.
  - `ClaimDueRun` resumes a parked run only on the node that owns its run session.
  - Step runs record the node too.
- [ ] **Postgres claim.** The Postgres variant of `ClaimRun` and `ClaimDueRun` adds `FOR UPDATE SKIP LOCKED` to the subquery, behind the same interface. It is the store's only dialect-specific query: it sits next to the portable one, and both pass the same store tests.
- [ ] **Roles and the primary lease.**
  - `icaro serve --role primary|runner` (`primary` is the default).
  - A cluster needs `database.driver: postgres` with `sslmode=verify-full`, refused otherwise.
  - The primary holds a lease row renewed with its heartbeat. A second primary on the same database refuses to start and names the holder.
  - A runner starts only the engine and the components the engine needs: run sessions, egress proxies, the repository cache, and secret resolution. It starts no API, UI, MCP, scheduler, webhook, preview, interactive session or memory listener, and opens no listening socket.
  - Wiring is per role in `internal/app`.
- [ ] **Runner registration.**
  - A `runners` table: id, hostname, role, version, schema version, slots, labels, state (`active`, `draining`), started, and heartbeat.
  - Each node upserts its row and refreshes `heartbeat_at`.
  - `runner.slots` replaces `workflows.max_runs` on runners; labels are stored even if placement waits.
- [ ] **Published configuration.**
  - The primary publishes the workflow-relevant configuration (`profiles`, `limits`, `workflows.*`, `integrations.*`) into a versioned store row whenever its config changes. Each run records the version it started with.
  - Runners execute only with the published copy. Their own file is checked to hold only machine-local keys (DSN, `docker.host`, `runner.*`, paths); any other key fails `icaro serve --role runner` at start.
- [ ] **Secrets.**
  - `icaro cluster key export` writes the master key encrypted under a passphrase (scrypt and secretbox, as the keyring's password-file backend already does in `internal/secrets/keyring.go`, so no new dependency).
  - `icaro cluster key import` stores it in the runner's keychain, or in the password-protected fallback.
  - A run that needs a secret its node cannot decrypt fails with that reason.
- [ ] **Logs in the store.**
  - On runners, step logs are written as redacted chunks of at most 64 KiB into `log_chunks` bodies, with a migration in both dialects.
  - `runs.log_retention` prunes old bodies.
  - The primary reads chunks from the store and files from its own disk behind one log reader.
  - A single node keeps writing files.
- [ ] **Events over the store.**
  - The primary's SSE source for runs it does not execute polls status and log-chunk changes every `PollInterval` (1 s).
  - On Postgres, `LISTEN`/`NOTIFY` wakes it sooner, behind the same interface and optional.
  - Cancel requests, approvals, and decisions on `await` and `call` steps already go through the store, and need no new path.
- [ ] **Lost nodes.**
  - Any node whose own heartbeat is fresh checks the others. A node whose `runners.heartbeat_at` is older than `cluster.lease` has its running and parked runs marked `failed` with `runner <id> lost`, and the steps in flight `failed`. This is a compare-and-set on that heartbeat, so it happens once.
  - A node that comes back within the lease re-attaches as a restarted daemon does.
  - A node that comes back after the lease does not resume those runs, and logs why.
- [ ] **Version gating and operations.**
  - A node refuses to claim while its schema or engine version differs from the primary's, and shows why in `runners`.
  - `icaro runners list` shows id, host, role, version, slots, labels, load, state and heartbeat.
  - `icaro runners drain <id>` stops claims and lets runs finish.
  - `icaro runners rm <id> --yes` removes a drained or lost node.
  - The web UI shows the same on a Runners page, and each run shows the node it ran on.
  - `go run ./hack/gendocs` regenerates `docs/cli.md`.
- [ ] **Doctor.** Checks:
  - the database is Postgres with `verify-full` when runners exist;
  - one primary lease is held;
  - no node runs a different version;
  - no node is lost;
  - each runner can decrypt a test value;
  - a runner's file holds only local keys.

  They are mapped in the doctor coverage test, with a row in the [doctor checks](../03-security.md#icaro-doctor).
- [ ] **Multi-node test environment.** A compose file under `hack/cluster/` with Postgres, a primary and two runners, each with its own Docker-in-Docker or rootless Docker. It is used by the integration and chaos suites behind a `cluster` build tag, and run in a CI job.
- [ ] **Docs.**
  - A new `docs/runners.md`: why and when, setup with Postgres, key export and import, draining and upgrades, what stays on the primary, what a lost node means, and the trust a runner needs.
  - [Architecture §1, §4 and §6](../01-architecture.md#1-processes--ownership).
  - [Security](../03-security.md): runners as trusted machines, the database credentials, and the key.
  - The blueprint: ADR 0040 marked Accepted, ADR 0001's amendment, cut 2's wording, a decision-index row, and the deferred-designs row removed.

## Acceptance criteria

- [ ] With no runner configured, every existing unit, integration, e2e, chaos and load suite passes unchanged on SQLite and on Postgres. A daemon upgraded in place adopts its `"daemon"` runs and recovers them.
- [ ] A second primary on the same database refuses to start and names the lease holder. A runner refuses to start on SQLite, or without `verify-full`.
- [ ] A runner has no listening TCP or Unix socket other than its run sessions' internal ones. A test lists the process's sockets.
- [ ] With a primary and two runners, 200 queued runs of a 3-step workflow each run exactly once, spread over the three nodes.
  - `forbid`, `queue` and `replace` hold across nodes.
  - No node exceeds its slots.
  - A `-tags load` variant stays within a bound measured on the reference machine and added to [ADR 0024](../adr/0024-hardening-budget.md).
- [ ] Restarting one runner resumes and re-attaches only its own runs. The other nodes' runs are untouched.
- [ ] A run parked on an approval is resumed by its own node after the decision, never by another one.
- [ ] Killing a runner mid-step fails its runs with `runner <id> lost` once `cluster.lease` passes, and a re-run succeeds on another node. Bringing it back within the lease instead re-attaches and finishes the run.
- [ ] A runner whose local file sets a different egress allowlist refuses to start. A run records the published configuration version it used, and a profile change on the primary applies to the next run on every node.
- [ ] A runner without the imported key fails a run that needs a secret, with the reason. After `icaro cluster key import`, the same run succeeds, and the key was never sent over the network by Ícaro.
- [ ] The primary's UI and `icaro runs logs --follow` show a runner's step log live, at most one poll interval behind. Cancel from the primary stops a runner's run within one heartbeat interval.
- [ ] A runner on a different version shows as refusing to claim in `icaro runners list`, and claims nothing. `drain` stops new claims and lets running runs finish.
- [ ] Every new decision has unit tests. The claim, recovery, lease and lost-node paths have tests against Postgres. The multi-node scenarios above run in the `cluster` CI job. Store changes pass on SQLite and Postgres, and coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets.

## Where it lives

- Roles and wiring: `internal/app`, `internal/daemon`; config: `internal/config`.
- Claims, lease, runners, published config, log chunks: `internal/store` (both dialects).
- Engine recovery and claiming: `internal/workflow/engine`; SSE source: `internal/api` and `internal/workflow/events`.
- Key export and import: `internal/secrets`, `internal/cli`; doctor checks: `internal/doctor`.
- Multi-node environment: `hack/cluster/`.
