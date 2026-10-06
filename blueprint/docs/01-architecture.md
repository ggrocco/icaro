# Target architecture

## 0. The session spine

The **session** is the core primitive ([ADR 0002](adr/0002-session-spine.md)). Container, preview, and memory are facets of a session; the workflow engine orchestrates sessions.

```text
session (internal/session)
  ├── worktree   git worktree create / track / clean up (only owner of worktrees)
  ├── container  isolated agent runtime, per-profile home (§5)
  ├── preview    live URL for this worktree's app (§7)
  └── memory     scoped read/write over MCP (§8)
```

## 1. Processes & ownership

```text
CLI / Wails UI / HTTP API
          │
          ▼
    Daemon local (icaro serve)
          │
          ▼
   Session Manager  ── owns worktree + the facets below, per unit of agent work
          │
 ┌────────┼───────────────────────────────────────────────┐
 │        │                  │             │              │
 │ Container Manager    Workflow Engine  Preview Manager  Memory Engine
 │  (+ profile homes)   (orchestrates       │             (+ MCP server)
 │        │              sessions)          │              │
 │ Docker API/CLI       Scheduler         Docker+Traefik  SQLite
 └────────────────────────────────────────────────────────┘
```

### Single-process rule

One daemon per user ([ADR 0001](adr/0001-single-daemon.md)):

- Exclusive lock with `flock`/`syscall.Flock` on the file `~/.icaro/run/icaro.lock`.
- PID and socket in `~/.icaro/run/`; the PID is informational, the lock is the source of truth.
- The CLI tries to talk to the socket first. If it does not exist and the command requires the daemon, it starts `icaro serve --background`.
- Wails starts or attaches to the daemon. The Wails backend does not implement a second engine.
- `icaro doctor` detects and repairs orphaned state; the full check list is in [03-security](03-security.md#icaro-doctor).

## 2. Go module structure

```text
cmd/icaro/                    # self-built Cobra-like parsing or urfave/cli (TBD); no business logic
internal/app/                 # composition, lifecycle, dependency graph
internal/daemon/              # HTTP/Unix socket, shutdown and ownership
internal/session/             # CORE PRIMITIVE: worktree+container+preview+memory lifecycle, sessions table, startup reconciliation (ADR 0002, 0006)
internal/container/           # image, mount policy, exec, lifecycle
internal/creds/               # per-profile persistent home: golden seed + per-session overlay (ADR 0005)
internal/workflow/            # schema, DAG planner, executor, scheduler, logs (orchestrates sessions)
internal/preview/             # watcher, compose, Traefik labels, cleanup
internal/memory/              # indexing, retrieval, handoff, per-project ACL, MCP server (ADR 0004)
internal/store/               # migrations + repositories
internal/integration/         # manifest packs, webhook router, HTTP action executor (ADR 0007)
internal/platform/            # small macOS/Linux abstractions
internal/doctor/              # checks and remediations
web/                          # React/Vite, Wails bindings, UI served by the daemon
schemas/                      # versioned JSON Schema
```

## 3. Transport between UI, CLI and daemon

- **Daemon API:** HTTP over a Unix socket on macOS/Linux by default; TCP only with an explicit `--listen`, a mandatory token, and loopback as the default.
- **Wails UI:** minimal bindings for lifecycle and window opening; operational data uses the same HTTP contract as the CLI to avoid forking.
- **Events:** SSE for logs/status; one stream per run. Do not use WebSocket before proving it is needed.
- **Local auth:** `0700` permissions on directories; socket `0600`; a token for remote TCP.

## 4. Persistence

Database: `~/.icaro/config/icaro.db` — a single local SQLite file (`modernc.org/sqlite`, pure Go) accessed through a `database/sql` store interface, so it can point at any SQL-compatible database when one is configured. Backups are a copy/snapshot of that file.

Initial tables: `settings`, `images`, `projects`, `repositories`, `sessions`, `workflows`, `workflow_versions`, `runs`, `step_runs`, `log_chunks`, `preview_instances`, `webhook_deliveries`, `memories`, `memory_edges`, `usage_snapshots`, `migrations`.

- WAL enabled; single writer via the store goroutine and a command channel.
- Bulky logs in append-only files with an index in the database; do not store megabytes of stdout as a single SQL cell.
- **`sessions` is the parent row** ([ADR 0006](adr/0006-session-persistence-recovery.md)): `id`, `project_id`, `profile`, `worktree_path`, `container_id`, `preview_instance_id`, `status`, `created_at`, `updated_at`, optional `last_heartbeat`. `status` ∈ `creating | running | queued | stopped | orphaned | error`. `preview_instances` and the relevant `runs`/`step_runs` reference it.
- **Startup reconciliation:** on boot, every non-terminal session is probed (container, worktree, preview). The daemon re-attaches where possible and otherwise marks the session `orphaned` for `icaro doctor`. The same path lets an in-place upgrade re-attach to running containers instead of orphaning them.
- `queued` holds sessions over the concurrency cap; the cap itself is an [open item](02-decisions-risks.md#open-items).

## 5. Agent container

Image `ghcr.io/icaro-dev/agent-base:<version>`:

- Non-root user, UID/GID mapped when possible.
- `/workspace` is an explicit bind mount of the session's worktree.
- `asdf` pre-installed in the image; plugins/runtimes declared per profile and materialized during setup.
- Optional tools: Claude Code, Codex, Pi, Cursor Agent, GitHub Copilot CLI, etc. Never bake credentials into the image.
- **Credentials live in a persistent per-profile home** ([ADR 0005](adr/0005-persistent-home-egress.md)), stored under `~/.icaro/home/<profile>`. The user logs in once, and the resulting `~/.claude` (and equivalents) is reused across workflow steps and the dev container. The home is a **golden seed layer** that holds the credentials and is read-mostly, plus a **per-session writable overlay** for runtime writes. Concurrent sessions share the login and never corrupt one another's `~/.claude`. Only this managed home is mounted, never the host's real `~/.claude`.
- The container runs under the hardening baseline and the **mandatory egress allowlist** in [03-security](03-security.md#mandatory-policies). Stronger isolation (per-container microVMs) is evaluated in feasibility spike S5.

## 6. Workflow engine

Scope: the workflow engine **orchestrates sessions** ([ADR 0002](adr/0002-session-spine.md)). It is not a general-purpose local CI; that space is well served by `act`/Dagger and is explicitly out of scope. The MVP is a DAG of agent-centric steps with explicit data flow between them.

A workflow is a DAG validated by JSON Schema plus semantic validations: unique names, acyclicity, valid references, retry/timeout policies, repository dependencies, and permissions. **Data flow is part of the contract:** steps declare `outputs`, later steps reference them via `${{ steps.<id>.outputs.<key> }}`, and `env` is explicit. A workflow without defined data flow is incomplete (see [the schema](schemas/workflow.schema.json)).

Step model: a step takes exactly one of two forms ([ADR 0007](adr/0007-integration-manifests.md)):

- An **`image` step** runs a container image (`image` + `command`) — the general compute primitive. An **agent** step runs the agent base image (§5) with the agent CLI as its command; a **shell** or **JS** step is just an image with the right runtime; host execution requires an explicit flag.
- A **`uses` step** invokes a declarative integration action (`uses: slack.post-message` + typed `input`) executed **in-process by the daemon** — talking to an external API needs no container. Notifications are `uses` actions, not a special step type.
- Both forms are full DAG citizens: `needs`, `when`, `retry`, `timeout`, and `outputs` behave identically. **Conditional execution** is a step's `when` expression, not a dedicated condition step.
- Runs started by an integration trigger expose the normalized event payload as `${{ trigger.* }}` (§9); manual runs prompt for those fields, so every workflow stays testable by hand.

Images are free to choose; the engine never bakes credentials into them. Agent credentials come from the profile home (§5); integration credentials stay host-bound in the daemon (§9).

Two contract details are still open (see [open items](02-decisions-risks.md#open-items)): the `when` grammar, and how an `image` step maps onto a session.

Executor:

- The planner turns the DAG into a schedule; each step receives a `context.Context`, a timeout, and cancellation.
- Configurable global and per-workflow parallelism.
- The repository is resolved once per `repository_ref + revision`; a temporary worktree is shared only by steps of the same run.
- An idempotency key for webhook/schedule; an explicit concurrency policy: `forbid`, `queue`, `replace`, `allow`.

## 7. Previews

- A watcher observes the configured root; each directory with a `compose.yaml` or `docker-compose.yml` becomes a candidate.
- Safe name: a slug of the path plus a short hash to avoid collisions; a human-readable hostname `slug.test` only when it does not collide.
- Proxy: Traefik as a container managed by Ícaro, connected to a dedicated external network `icaro-preview`.
- Port discovery: requires the `icaro.preview.port` label; falls back to `80` only if unambiguous. Do not guess exposed ports.
- Routing: Ícaro generates a Compose override with Traefik labels, without editing the user's compose. The compose file itself must pass the [compose lint](03-security.md#mandatory-policies).
- `.test`: installs local resolution via dnsmasq/CoreDNS only with consent; a predictable alternative is `http://localhost:<port>`.
- Public HTTPS: a custom domain + a supported DNS provider; a wildcard cert with DNS-01. Never promise Let's Encrypt for `.test`.

**Container↔preview seam** (the hardest integration, owned by `session`). When the agent edits code inside its own container, the preview must reflect the change:

- **Topology:** the agent and the app's dev server run in **separate containers**, and both bind-mount the **same host worktree path**. The agent's `/workspace` and the preview's compose build context are the same directory.
- **Propagation:** the project is **bind-mounted, not copied**. Hot reload is the **app dev server's** job, not Ícaro's. Ícaro only restarts the preview when its configuration changes (compose file, env, lockfile).
- **macOS:** on Docker Desktop, data stays coherent across the two VirtioFS hops, but the preview's in-container watcher does not reliably receive inotify events. **Polling watch is mandatory on macOS** (`CHOKIDAR_USEPOLLING`, Vite `server.watch.usePolling`, etc.). Feasibility spike S1 confirms it.
- **MVP scope:** compose-based projects with a declared `icaro.preview.port`. Host-run dev servers (Vite/Next on the host) are out of scope.

## 8. Shared memory

Do not copy Graphify or ai-memory as a core dependency. Integrate via adapters/importers and reproduce only compatible concepts:

- deterministic indexing: files, symbols, imports, commits, ADRs;
- human/agent memory: decisions, tasks, findings, and handoffs;
- strict scoping: `project_id`, branch/worktree, agent, origin, and TTL;
- hybrid retrieval: text/FTS first; vectors only after a local benchmark;
- every memory returned carries provenance, a timestamp, and a link to the source.

**Agent interface** ([ADR 0004](adr/0004-memory-mcp.md)): the daemon hosts an **MCP server** and injects it into each session's agent config at setup. It exposes three tools:

- write a decision, task, finding, or handoff;
- query with FTS plus filters;
- read provenance.

Scope is enforced server-side from the session identity. Deterministic ingest runs without the agent. Non-MCP agents use a documented HTTP/CLI fallback. How the MCP endpoint reaches the agent inside its container is feasibility spike S3.

## 9. Integrations

An integration is a **declarative manifest in a git-backed pack**, not code ([ADR 0007](adr/0007-integration-manifests.md)). Out-of-v1 limits are in [MVP cut 9](02-decisions-risks.md#mandatory-mvp-cuts).

- **Manifests.** A YAML manifest declares two things, and manifests execute no code:
  - **triggers:** webhook signature verification, event matching, and payload normalization into the typed `${{ trigger.* }}` context;
  - **actions:** typed inputs, an HTTP request template, auth by reference, and output extraction from the response.

  Manifests and workflows share one interpolation language.
- **Packs are git repositories** under `~/.icaro/integrations/`:
  - `builtin` is read-only and embedded in the binary. It ships GitHub, GitLab, Slack, Jira, Linear, Sentry, and Datadog, plus a generic `webhook` integration for custom sources.
  - Third-party packs are cloned with `icaro integration add <git-url>`.
  - The `local` pack is auto-initialized as a git repo on first edit, and **UI edits commit directly to it**. Push is manual by default.
  - A user pack may deliberately shadow a builtin; a collision between two user packs is a validation error.
  - The daemon watches the packs. An invalid manifest keeps its last good version loaded.
- **Engine surface.** Exactly three generic pieces:
  - a webhook endpoint per integration (`/hooks/<name>`);
  - an in-daemon HTTP action executor for `uses:` steps;
  - the `trigger.*` context.

  An inbound event is handled in this order: verify the signature per the manifest, match subscribed workflows, evaluate `filter`, normalize the payload, then enqueue with the executor's idempotency key and concurrency policy (§6).
- **Reachability.** Inbound webhooks assume a reachable daemon (homelab/VPS with a hostname). Polling is the documented laptop fallback; the manifest model allows for it, but it is not built in the MVP.
- **Credentials and network.** Integration credentials are host-bound and never enter a container. The executor enforces an SSRF guard, and installing a pack shows a permission summary. These controls are policies in [03-security](03-security.md#mandatory-policies).
