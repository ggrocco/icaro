# Target architecture

## 0. The session spine

The **session** is the core primitive (ADR 0002). Container, preview, and memory are facets of a session, not independent subsystems; the workflow engine orchestrates sessions. Read the rest of this document through that lens.

```text
session (internal/session)
  ├── worktree   git worktree create / track / clean up
  ├── container  isolated agent runtime (creds via broker)
  ├── preview    live URL for this worktree's app
  └── memory     scoped read/write over MCP
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
 │  (+ creds broker)    (orchestrates       │             (+ MCP server)
 │        │              sessions)          │              │
 │ Docker API/CLI       Scheduler         Docker+Traefik  SQLite
 └────────────────────────────────────────────────────────┘
```

### Single-process rule

- Exclusive lock with `flock`/`syscall.Flock` on the file `~/.icaro/run/icaro.lock`.
- PID and socket in `~/.icaro/run/`; the PID is informational, the lock is the source of truth.
- The CLI tries to talk to the socket first. If it does not exist and the command requires the daemon, it starts `icaro serve --background`.
- Wails starts or attaches to the daemon. The Wails backend does not implement a second engine.
- `icaro doctor` checks for an orphaned lock, a non-existent PID, an unreachable socket, Docker, local DNS, permissions, and migrations.

## 2. Go module structure

```text
cmd/icaro/                    # self-built Cobra-like parsing or urfave/cli (TBD); no business logic
internal/app/                 # composition, lifecycle, dependency graph
internal/daemon/              # HTTP/Unix socket, shutdown and ownership
internal/session/             # CORE PRIMITIVE: worktree+container+preview+memory lifecycle (ADR 0002)
internal/container/           # image, mount policy, exec, lifecycle
internal/creds/               # credential broker: scoped/short-lived access for agents (ADR 0003)
internal/workflow/            # schema, DAG planner, executor, scheduler, logs (orchestrates sessions)
internal/preview/             # watcher, compose, Traefik labels, cleanup
internal/memory/              # indexing, retrieval, handoff, per-project ACL, MCP server (ADR 0004)
internal/store/               # migrations + repositories
internal/notify/              # Slack/email/webhook
internal/integration/         # GitHub/GitLab webhooks
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

Database: `~/.icaro/config/icaro.db`.

Initial tables: `settings`, `images`, `projects`, `repositories`, `workflows`, `workflow_versions`, `runs`, `step_runs`, `log_chunks`, `preview_instances`, `webhook_deliveries`, `memories`, `memory_edges`, `usage_snapshots`, `migrations`.

- WAL enabled; single writer via the store goroutine and a command channel.
- Bulky logs in append-only files with an index in the database; do not store megabytes of stdout as a single SQL cell.
- Default storage is a single local SQLite file; backups are a copy/snapshot of that file. Access goes through a `database/sql` store interface, so it can point at any SQL-compatible database when one is configured.

## 5. Agent container

Image `ghcr.io/icaro-dev/agent-base:<version>`:

- Non-root user, UID/GID mapped when possible.
- `/workspace` an explicit bind mount per project; persistent home in `~/.icaro/home/<profile>`.
- `asdf` pre-installed in the image; plugins/runtimes declared per profile and materialized during setup.
- Optional tools: Claude Code, Codex, Pi, Cursor Agent, GitHub Copilot CLI, etc. Never bake credentials into the image.
- Credentials: **brokered, not raw-mounted** (ADR 0003). The agent talks to a credential broker over the session socket and receives scoped, short-lived access; no bind mount of `~/.claude`/`~/.config/gh`/`~/.aws` by default. A raw mount requires an explicit profile and a setup-time warning.
- No `--privileged`, no Docker socket, no host network by default, capabilities drop all, default seccomp.
- Stronger isolation is a first-class option, not just "later": evaluate per-container microVMs (Apple Containerization on Apple Silicon, gVisor/Firecracker on Linux) in the Phase 0/1 spike. Untrusted profiles get a **mandatory** egress allowlist (see `docs/03-security.md`).

## 6. Workflow engine

Scope: the workflow engine **orchestrates sessions** (ADR 0002). It is not a general-purpose local CI; that space is well served by `act`/Dagger and is explicitly out of scope. The MVP is a DAG of agent-centric steps with explicit data flow between them.

A workflow is a DAG validated by JSON Schema plus semantic validations: unique names, acyclicity, valid references, retry/timeout policies, repository dependencies, and permissions. **Data flow is part of the contract:** steps declare `outputs`, later steps reference them via a defined interpolation syntax, and `env` is explicit. A workflow without defined data flow is incomplete (see `docs/schemas/workflow.schema.json`).

MVP step types:

1. `agent`: runs a CLI in the container.
2. `shell`: runs a Bash command in the container, or on the host only with an explicit flag.
3. `javascript`: runs a JS runtime in a sandboxed container.
4. `notify`: Slack/email/webhook.
5. `condition`: a simple declarative gate.

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
- Routing: Ícaro generates a Compose override with Traefik labels, without editing the user's compose.
- `.test`: installs local resolution via dnsmasq/CoreDNS only with consent; a predictable alternative is `http://localhost:<port>`.
- Public HTTPS: a custom domain + a supported DNS provider; a wildcard cert with DNS-01. Never promise Let's Encrypt for `.test`.
- **Container↔preview seam (hardest integration, owned by `session`).** When the agent edits code inside its own container, the preview must reflect those edits. The worktree is the shared surface: the agent's `/workspace` and the preview's compose build context resolve to the **same worktree path**, so file changes are visible to both and the app's existing watch/hot-reload picks them up. MVP scope is **compose-based projects with a declared `icaro.preview.port`**; host-run dev servers (Vite/Next on the host) are out of MVP scope and documented as such. A spike must confirm file-event propagation works across the container boundary on macOS (Docker Desktop file sharing) before this is promised.

## 8. Shared memory

Do not copy Graphify or ai-memory as a core dependency. Integrate via adapters/importers and reproduce only compatible concepts:

- deterministic indexing: files, symbols, imports, commits, ADRs;
- human/agent memory: decisions, tasks, findings, and handoffs;
- strict scoping: `project_id`, branch/worktree, agent, origin, and TTL;
- hybrid retrieval: text/FTS first; vectors only after a local benchmark;
- every memory returned carries provenance, a timestamp, and a link to the source.

**Agent interface (ADR 0004).** Memory is exposed to agents as an **MCP server** hosted by the daemon, injected into each session's agent config at setup. Tools: write (decision/task/finding/handoff), query (FTS + filters), read-provenance. Scope is enforced **server-side** from the session identity — an agent cannot read another project's memory by asking. Deterministic ingest runs without the agent; the MCP surface is for human/agent memory and retrieval. Non-MCP agents fall back to a documented HTTP/CLI interface.
