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
- **One service layer.** The CLI, HTTP API, MCP server (§10), and UIs are thin clients over the same daemon service layer; business rules live only there.
- **Local auth:** single user. `0700` permissions on directories and a `0600` socket protect local access. TCP requires an API token, stored hashed and scoped `read`, `write`, or `admin`. The browser UI logs in by pasting a token, which creates an `HttpOnly`, `SameSite=Strict` cookie session; cookie-authenticated mutations also need a custom request header (CSRF). How the browser reaches a daemon that listens only on a Unix socket by default is [open item O5](02-decisions-risks.md#open-items). Multi-user accounts/OIDC come later, behind the same session table.

## 4. Persistence

Database: `~/.icaro/config/icaro.db` — a single local SQLite file (`modernc.org/sqlite`, pure Go) accessed through a `database/sql` store interface. Backups are a copy/snapshot of that file. Postgres is the supported alternative when one is configured. To keep the swap honest, the store uses portable SQL only (no dialect-only features; JSON stored as text), keeps migrations per dialect, and CI runs the store tests against both.

Initial tables: `settings`, `images`, `projects`, `repositories`, `sessions`, `workflows`, `workflow_versions`, `runs`, `step_runs`, `log_chunks`, `preview_instances`, `webhook_deliveries`, `memories`, `memory_edges`, `usage_snapshots`, `migrations`.

- WAL, `busy_timeout`, and foreign keys enabled; single writer via the store goroutine and a command channel.
- Bulky logs in append-only files with an index in the database, secret-redacted as they are written; do not store megabytes of stdout as a single SQL cell.
- Runs are queued in the database and claimed transactionally, so a restart never loses or double-starts one.
- **`sessions` is the parent row** ([ADR 0006](adr/0006-session-persistence-recovery.md)): `id`, `project_id`, `profile`, `worktree_path`, `container_id`, `preview_instance_id`, `status`, `created_at`, `updated_at`, optional `last_heartbeat`. `status` ∈ `creating | running | queued | stopped | orphaned | error`. `preview_instances` and the relevant `runs`/`step_runs` reference it.
- **Startup reconciliation:** on boot, every non-terminal session is probed (container, worktree, preview). The daemon re-attaches where possible and otherwise marks the session `orphaned` for `icaro doctor`. The same path lets an in-place upgrade re-attach to running containers instead of orphaning them. To make re-attach possible, a step's container id is persisted **before** the container starts, containers are labeled with their run, step, and attempt, and log capture resumes from the recorded offset.
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

Validation returns structured issues: a JSON Pointer path, a message, and a nearest-match hint for unknown names. The CLI, API, UI, and the MCP `validate_workflow` tool all show the same list. The implementation generates its published schema from its own types, and a drift test keeps that output equal to [the blueprint contract](schemas/workflow.schema.json).

Step model: a step takes exactly one of two forms ([ADR 0007](adr/0007-integration-manifests.md)):

- An **`image` step** runs a container image (`image` + `command`) — the general compute primitive. An **agent** step runs the agent base image (§5) with the agent CLI as its command; a **shell** or **JS** step is just an image with the right runtime; host execution requires an explicit flag.
- A **`uses` step** invokes a declarative integration action (`uses: slack.post-message` + typed `input`) executed **in-process by the daemon** — talking to an external API needs no container. Notifications are `uses` actions, not a special step type.
- Both forms are full DAG citizens: `needs`, `when`, `retry`, `timeout`, and `outputs` behave identically. **Conditional execution** is a step's `when` expression, not a dedicated condition step.
- Runs started by an integration trigger expose the normalized event payload as `${{ trigger.* }}` (§9); manual runs prompt for those fields, so every workflow stays testable by hand.

Images are free to choose; the engine never bakes credentials into them. Agent credentials come from the profile home (§5); integration credentials stay host-bound in the daemon (§9).

**Image-step I/O contract.** Any container that honors this contract is a valid step; writing one needs no knowledge of Ícaro's internals.

| Channel | Mechanism |
|---|---|
| Inputs | `ICARO_INPUT_<NAME>` environment variables plus `/icaro/input.json`, holding the step's resolved `input` |
| Outputs | The step writes a JSON object to `/icaro/output.json`, capped at 1 MiB. Each declared `outputs` entry says how its value is captured, per the schema (a JSON path into that file, stdout, or another file) |
| Files | `/workspace` — the run's worktree, shared by the steps of that run |
| Status | Exit code `0` = success |
| Logs | stdout/stderr, captured and secret-redacted |
| Secrets | `${{ secrets.<key> }}` in `env`, resolved by the daemon; values are redacted from logs |

Agent CLIs run headless in workflows: non-interactive mode, prompt in, transcript to the logs, structured result to `/icaro/output.json`.

Two contract details are still open (see [open items](02-decisions-risks.md#open-items)): the `when` grammar, and how an `image` step maps onto a session.

Executor:

- The planner turns the DAG into a schedule; each step receives a `context.Context`, a timeout, and cancellation.
- Configurable global and per-workflow parallelism.
- The repository is resolved once per `repository_ref + revision`; a temporary worktree is shared only by steps of the same run.
- An idempotency key for webhook/schedule; an explicit concurrency policy: `forbid`, `queue`, `replace`, `allow`.
- The scheduler persists each cron trigger's next fire time; a fire missed while the daemon was down runs once on restart, not once per missed tick.

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

Handoffs only help if agents use them. Each agent session gets a short, configurable preamble telling it to accept a pending handoff before starting and to leave one when done.

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

  An inbound event is handled in this order: verify the signature per the manifest, match subscribed workflows, evaluate `filter`, normalize the payload, then enqueue with the executor's idempotency key and concurrency policy (§6). Filtering happens before enqueueing, so a filtered-out event never starts a container.
- **Verification kinds.** A manifest's trigger declares how requests are verified. This covers HMAC signatures (GitHub's `X-Hub-Signature-256`) and shared-secret headers (GitLab's `X-Gitlab-Token`).
- **Delivery records.** Every inbound request is persisted with its verification result, matched workflows, and resulting runs, and can be **redelivered** from that record. The record is the dedup point for providers that retry, and it answers "why didn't my workflow fire?".
- **Reachability.** Inbound webhooks assume a reachable daemon (homelab/VPS with a hostname). Polling is the documented laptop fallback; the manifest model allows for it, but it is not built in the MVP. Until then, a polling source is a `cron` workflow whose first step checks the source and gates the remaining steps with `when`.
- **Credentials and network.** Integration credentials are host-bound and never enter a container. The executor enforces an SSRF guard, and installing a pack shows a permission summary. These controls are policies in [03-security](03-security.md#mandatory-policies).

## 10. Agent interface

AI agents author and run workflows through the same daemon MCP server that hosts memory (§8, [ADR 0004](adr/0004-memory-mcp.md)). The design goal is that authoring converges: agents invent config fields, and a published schema plus a precise validator turns guesswork into a loop.

- **Resources:** the workflow JSON Schema, and one input schema per integration action, compiled from its manifest (§9). An agent therefore knows an action's required inputs before writing YAML.
- **Tools:**
  - `get_workflow_schema`;
  - `list_integrations` / `get_integration`;
  - `list_secrets` — names and host bindings only, **never values**;
  - `validate_workflow` — the structured issues from §6. This is the most important tool, because issue quality decides how fast an agent converges;
  - `create_workflow` / `update_workflow` / `get_workflow` / `list_workflows`;
  - `run_workflow`;
  - `get_run` — status, per-step outputs, and a size-capped log tail, so debugging does not flood the agent's context.
- **No destructive tools in v1.** Agents author and run; humans delete.
- **Transports:** stdio (`icaro mcp`, which proxies to the daemon) for local CLIs, and streamable HTTP on the daemon under the same auth as the API (§3). One tool-definition layer sits over the service layer, so both transports share semantics.
- **Authoring skill.** An `icaro-workflows` skill ships in the repo, versioned with the engine. It is installable into Claude Code/Codex with `icaro skill install`, which writes the skill and the MCP entry. It encodes the loop:
  1. re-fetch the schema and integrations; never write from memory;
  2. list secrets, and ask the human to create missing ones;
  3. draft, then `validate_workflow` until the workflow is clean;
  4. create it, then run it with a small input;
  5. debug with `get_run`.

Exposing these tools *inside* agent containers, so a running agent can invoke or author workflows, is a [deferred design](02-decisions-risks.md#deferred-designs).
