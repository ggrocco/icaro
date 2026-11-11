# ADR 0040 — Multi-runner scale-out: runners pull workflow runs from a shared Postgres

**Status:** Proposed. Would amend [ADR 0001](0001-single-daemon.md) (one daemon per user becomes one **primary** per database, plus optional runners) and narrow [MVP cut 2](../02-decisions-risks.md#mandatory-mvp-cuts). Takes the "multi-runner scale-out" row out of the [deferred designs](../02-decisions-risks.md#deferred-designs).

## Context

One daemon runs every workflow on one machine's Docker. `workflows.max_runs` and `workflows.concurrency` bound it, and a busy laptop or a single lab box is the ceiling. Users who already run a shared daemon behind `--listen` ([ADR 0034](0034-remote-hosts-and-trusted-loopback.md)) ask to add machines for runs (bigger builds, an `arm64` box), without splitting their workflows across daemons.

The deferred-design row noted that "the DB claim can move to `FOR UPDATE SKIP LOCKED` on Postgres behind the same interface". The store is close to ready:

- runs are claimed with a compare-and-set update that records `claimed_by` and `claimed_at`;
- running runs refresh `heartbeat_at`, and the same loop picks up cancel requests;
- Postgres is a supported store, and CI runs the store tests on both dialects.

Four things in the code assume one daemon, and a design has to answer each:

1. **Identity.** The engine claims as the constant runner id `"daemon"`, and on start it resumes *every* `running` run.
2. **Machine-local state.** A run's session, workspace and repository worktrees ([ADR 0010](0010-run-sessions.md)) and its containers live on one Docker host. Step logs are append-only files on that host, with an index in the database.
3. **Secrets.** Secret values are encrypted under a master key held in the host's keychain.
4. **Configuration.** Profiles, egress allowlists and `limits` come from each machine's own YAML file.

[Cut 2](../02-decisions-risks.md#mandatory-mvp-cuts) also says: no remote execution over TCP beyond loopback.

## Decision

### Roles: one primary, any number of runners

- **The primary** is the daemon of ADR 0001, unchanged in what it serves:
  - the API, the UI and MCP;
  - interactive sessions, previews and memory;
  - the scheduler and webhooks;
  - and it runs workflow runs itself unless configured not to.
- **A runner** is `icaro serve --role runner` on another machine. It serves no API, UI, sessions, previews, scheduler or webhooks. It only claims workflow runs from the database and executes them on its own Docker, with its own egress proxies, run sessions and repository cache.
- **Postgres is required** for a cluster: every node uses the same DSN, with `sslmode=verify-full`. SQLite stays single-node.
- **One primary per database.** The primary holds a lease row in the store, renewed with its heartbeat. A second primary on the same database refuses to start, as a second daemon on one machine already does through the `flock`. ADR 0001 becomes: one primary per database, and at most one daemon per machine.

**No inbound port on a runner, and no command over the network.** A runner connects out to Postgres and to its steps' egress destinations, nothing else. The primary never calls a runner. Work moves only through rows a runner claims, and cancellation through the `cancel_requested_at` the heartbeat already reads. Cut 2 is narrowed, not dropped: nothing executes a command received over TCP.

### Runner identity and claiming

- A runner has a stable id, generated on first start and kept in its local `runner_meta`, plus its hostname, version, `runner.slots` (its `max_runs`) and optional labels. It registers in a new `runners` table and refreshes `heartbeat_at` there.
- Every claim records the claiming runner's id. On start, a node resumes and re-attaches only the runs it claimed itself. It never takes another node's runs.
- The claim stays `ClaimRun` / `ClaimDueRun` behind the same store interface. On Postgres the subquery adds `FOR UPDATE SKIP LOCKED`, so runners polling together do not contend on one row. That is the store's one dialect-specific query, kept next to the portable one and tested on both.
- The concurrency policies (`forbid`, `queue`, `replace`) are already enforced in the claim's SQL, so they hold across nodes without change.

### What stays on one machine

A run is **pinned to the node that claimed it** for its whole life. Its workspace, worktrees and containers are on that node's disk and Docker.

- **Parked runs resume on their node.** A run waiting on an approval, `wait`, `await` or `call` is claimed again only by the node that owns its run session (`ClaimDueRun` filters on it).
- **A lost node.** When a node's heartbeat in `runners` is older than `cluster.lease` (default 60 s):
  - its running and parked runs are marked `failed` with `runner <id> lost`, by whichever node notices first, with a compare-and-set on the node's last heartbeat;
  - the steps in flight are recorded `failed`, as for a step container lost while the daemon was down ([ADR 0024](0024-hardening-budget.md));
  - nothing is retried on another node, because the workspace is gone with the node.

  **Re-run** starts them again from scratch. A node that comes back within the lease re-attaches as a restarted daemon does today.
- **Interactive sessions, previews, `icaro login` and profile homes stay on the primary.** Scale-out is for workflow runs only.

### Configuration comes from the primary

Workflow-relevant configuration is **published by the primary into the store**, and runners execute with that copy, never with their own:
- `profiles` (image, egress allowlist, runtime, `inject`);
- `limits`;
- `workflows.*`;
- `integrations.*`.

Each run records the configuration version it started with. A runner's own file holds only what is local to its machine: the DSN, `docker.host`, `runner.slots`, labels and paths. So a profile's egress allowlist is the same wherever a run lands, and editing it is still an operator change in one place.

### Secrets

Runners resolve `${{ secrets.* }}`, integration credentials for `uses` steps, and `inject` keys, so they need the master key.

- `icaro cluster key export` on the primary writes the key encrypted under a passphrase.
- `icaro cluster key import` on the runner stores it in that machine's keychain.

The key never crosses the network through Ícaro. A runner is therefore **as trusted as the primary for workflow execution**, and the docs and `icaro doctor` say so. A runner that cannot decrypt a secret its run needs fails the run with that reason; it does not skip the step.

### Logs and events

- Runners write step logs, redacted as today, to the store as **bounded chunks** (64 KiB each) instead of local files, so the primary can serve them. `webhooks.retention`-style pruning applies through a new `runs.log_retention`. A single-node daemon keeps writing files.
- The primary's SSE streams for a runner's runs read new chunks and status changes from the store. They poll, and on Postgres use `LISTEN`/`NOTIFY` as an optimisation behind the same interface. Latency is one poll interval at worst (1 s).

### Versions, draining and limits

- A node refuses to claim when its schema version or engine version differs from the primary's, recorded in the store. An upgrade is: drain the runners, upgrade the primary, upgrade the runners.
- `icaro runners list` shows nodes, slots, labels, load and heartbeat. `icaro runners drain <id>` stops a node claiming new runs and lets its runs finish. `icaro runners rm <id>` (with `--yes`) removes a drained or lost node.
- `workflows.max_runs` becomes per node (`runner.slots` on runners). The primary's `sessions.max_active` ([ADR 0031](0031-session-concurrency-cap.md)) is unchanged.
- **Placement** in this ADR is "any node with a free slot". Label-based placement (a workflow asking for `arm64`) is an open question below.

## Alternatives considered

- **The primary drives remote Docker hosts** (`docker.host` over TCP or SSH). One daemon, no new role. But bind-mounted workspaces and worktrees would have to exist on each remote host, the Docker API over the network is root-equivalent on that host, and the egress proxy and its internal network would sit on another machine than the daemon that configures them.
- **A push API from the primary to runners.** The primary would assign runs over TCP. It needs a new authenticated listener on every runner, which is exactly what cut 2 avoids. Pulling from the database uses a channel the nodes already need.
- **Kubernetes or another orchestrator as the executor.** A large dependency, and the one the [prior-art table](../02-decisions-risks.md#prior-art) lists to avoid when borrowing from Argo Workflows.
- **Moving runs between nodes on failure.** It would need a shared filesystem or a workspace snapshot for every step. Failing with a clear reason and re-running is honest and cheap.

## Consequences

- Workflows scale past one machine with no change to workflow files, and a run behaves the same on any node, because its configuration comes from the primary.
- A cluster needs Postgres, and every runner holds the database credentials and the master key. A runner is a trusted machine, not a sandbox for untrusted work; containers on it are still confined as everywhere else.
- A lost node fails its runs instead of moving them. `cluster.lease` trades false alarms against how long a dead node's runs hang.
- New state:
  - tables: `runners`, the primary lease, and the published configuration with versions;
  - columns: the claiming runner on runs and step runs, chunk bodies in `log_chunks`.
- New commands: `icaro serve --role runner`, `icaro runners`, and `icaro cluster key export|import`.
- The engine's startup recovery, the claim loop, log writing and the SSE source change for every user, including single-node ones. A single node must behave exactly as today, and its tests say so.
- Implementation would be a new phase: roles and the primary lease, runner registration and claims, recovery by owner, the published configuration, key export and import, logs in the store, SSE over the store, lost-node handling, version gating, draining, docs, and a multi-node test environment in CI.

## Open questions

1. Should label-based placement (a workflow-level `runs_on`, matched against runner labels) be in v1, or come later?
2. What should `cluster.lease` default to, and should a lost node's runs fail (as drafted) or be re-queued from scratch automatically?
3. Should log bodies go in Postgres chunks (as drafted), or to an object store the nodes share?
4. Should runners hold the master key (as drafted), or a separate key that decrypts only what workflows may use, re-encrypted by the primary?
