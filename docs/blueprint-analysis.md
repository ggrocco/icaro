# Icaro — Blueprint Analysis

> Status: pre-implementation analysis. This document reviews the core concept and
> proposes an architecture, with special focus on the identified weak point:
> **an easy way to build integrations (triggers + external API calls) for workflow steps.**

## 1. The concept

Icaro is a workflow engine where:

- A **workflow** is a sequence (or graph) of **steps**.
- Each **step** runs a user-provided script inside a **Docker container**.
- Workflows should be able to be **triggered** by external events and to **call
  external APIs** from steps.
- The system should stay **simple**, but adding a **new integration must be easy** —
  no forking the engine, no heavy SDK.
- **AI agents are first-class users**: an MCP server exposes the engine (including
  the workflow JSON Schema) so agents can author workflows precisely, and a shipped
  skill teaches them the authoring loop (§5).
- Icaro is the **server-side implementation of the
  [ai-launcher](https://github.com/lgldsilva/ai-launcher) concept**: where
  ai-launcher interactively launches AI CLI harnesses (claude, codex, opencode, …)
  sandboxed on a dev machine, Icaro runs the same harnesses headless, as workflow
  steps, triggered by events (§2.5).

This is a proven shape: Drone CI, GitHub Actions, and Argo Workflows all converge on
"a step is a container run." The concept is sound. The part that kills projects like
this is usually not the executor — it is the integration surface. That is exactly the
part flagged as under-addressed, so most of this document is about it.

## 2. The key design decision: integrations are not a separate subsystem

The trap to avoid is the **n8n/Zapier model**: every integration is a bespoke "node"
written against an internal SDK, with its own UI code, registered inside the platform.
That gives a nice catalog but makes each integration expensive to build and couples it
to the engine's release cycle.

The recommended model instead: **an integration is just a step, packaged as a Docker
image plus a small manifest.** The engine never knows what "Slack" or "GitHub" is —
it only knows the step contract.

### 2.1 The step contract (the heart of the system)

Every step — user script or published integration — obeys one uniform contract:

| Channel | Mechanism |
|---|---|
| Inputs | Environment variables (`ICARO_INPUT_*`) + a JSON file at `/icaro/input.json` |
| Outputs | Write JSON to `/icaro/output.json` (available to later steps) |
| Shared files | A workspace volume mounted at `/workspace` across all steps of a run |
| Status | Exit code `0` = success, non-zero = failure |
| Logs | stdout/stderr, captured by the runner |
| Credentials | Injected as env vars from a named **connection** (see 2.3) |

This is the Drone CI plugin contract, and it is the reason Drone has hundreds of
community plugins written in bash, Go, Python, whatever: **writing an integration
requires zero knowledge of the engine's internals.** Any container that reads env
vars and exits 0/1 is already a valid Icaro step.

### 2.2 The integration manifest

An integration = **Docker image + `integration.yml`**:

```yaml
# integration.yml — packaged with (or referenced by) the image
name: slack-message
version: 1.2.0
description: Send a message to a Slack channel
image: ghcr.io/icaro-integrations/slack-message:1.2.0
connection: slack            # type of credential it needs
inputs:
  channel:  { type: string, required: true }
  text:     { type: string, required: true }
  thread_ts:{ type: string, required: false }
outputs:
  message_ts: { type: string }
```

The manifest exists only so the engine (and a future UI) can validate inputs and
render forms. The runtime behavior is still just "run the image with env vars."

Distribution can start with zero infrastructure: a git repo of manifests
(`icaro-integrations/`) that the engine syncs, exactly like Homebrew taps or Helm
repos. No registry service to build in the MVP.

**Making a new integration is therefore:** write a 30-line script, `docker build`,
write ~20 lines of YAML. That satisfies the "simple but easy to add new integrations"
goal directly.

### 2.3 Connections (credentials) as first-class objects

Integrations are useless without a sane credential story. Model a **connection** as a
named, typed secret bundle stored by the engine (encrypted at rest):

```yaml
# created once via API/CLI: icaro connection create slack-prod --type slack
type: slack
fields:
  bot_token: xoxb-...
```

A workflow step references `connection: slack-prod`; the runner injects the fields as
`ICARO_CONN_BOT_TOKEN`, etc. Benefits:

- Secrets never live in workflow definitions.
- One connection is reused across many workflows.
- The manifest's `connection: slack` field lets the engine validate that a step got a
  connection of the right type.

### 2.4 The escape hatch: generic built-in steps

Ship 3–4 built-in step types so people can integrate with *anything* before a packaged
integration exists:

- `run` — arbitrary script on a chosen image (the core feature).
- `http` — declarative HTTP request (URL, method, headers from connection, JSON body,
  capture response into outputs). This alone covers ~70% of "call an external API."
- `wait` / `approval` — pause until a callback or human approval (optional, later).

A packaged integration is then just a *nicer* version of what users can already do
with `http` or `run` — the catalog can grow lazily, driven by real use.

### 2.5 AI agent steps — the ai-launcher lineage

Icaro's defining workload is running **AI CLI harnesses as workflow steps**. This is
[ai-launcher](https://github.com/lgldsilva/ai-launcher)'s model moved server-side,
and the mapping is direct:

| | ai-launcher (local) | Icaro (server) |
|---|---|---|
| Where it runs | Dev machine, interactive TUI/PTY | Headless, one Docker container per step |
| Sandbox | ai-jail (bubblewrap/sandbox-exec) | §6.1 sandbox profiles (same philosophy) |
| Started by | A human at a keyboard | Webhooks, SCM events, cron, other agents |
| Harnesses | claude, codex, opencode, kimi, … | The same catalog, packaged as images |
| Trust model | Opt-in permission toggles, RO mounts | Deny-by-default schema, no host mounts |

Both share the same design principle ai-launcher states outright: *compose, don't
reimplement* — the harness is the intelligence; the platform provides launch,
confinement, and plumbing.

Concretely, a built-in **`agent` step type**:

```yaml
steps:
  - name: fix-flaky-test
    agent:
      harness: claude                 # from the harness catalog
      prompt: |
        Fix the failing test reported in the input payload.
        The repo is checked out in /workspace/repo.
      params: { model: sonnet }       # params the harness manifest declares
    connection: anthropic             # API key injected, never baked into images
    sandbox: strict                   # §6.1 — agents always run strict
```

Design decisions, each traceable to an ai-launcher feature:

- **Harness catalog** — mirrors ai-launcher's agent catalog: each harness is a
  packaged image (CLI preinstalled, **pinned by digest** — its SHA-256 checksum
  verification transposed to containers) plus a manifest declaring the parameters
  that harness accepts (ai-launcher's declared-params idea, e.g. Kimi's
  `query`/`model`). Distributed exactly like integrations (§2.2), validated through
  the same schema pipeline (§5.1), so agents authoring workflows know each
  harness's exact knobs.
- **Headless execution** — harnesses run in non-interactive mode (`claude -p`, and
  equivalents), prompt in, transcript to logs, structured result to
  `/icaro/output.json`. No PTY layer to build.
- **Permission toggles become schema fields** — what is a TUI checkbox in
  ai-launcher (SSH, Docker socket, GPU, display) is an explicit, validated field
  here, and strictly narrower: the Docker socket and host mounts are not grantable
  at all (§6.1); network and connections are the only capabilities a step can
  request. Host-desktop toggles (GPU/display) have no server equivalent and are
  simply not features.
- **`--dry-run` becomes `validate_workflow`** — same idea, same payoff: see the
  exact effective configuration before anything executes.
- **Profiles become reusable step presets** — a later, cheap feature: named,
  versioned `agent` step fragments (`preset: pr-reviewer`) referenced across
  workflows.

What ai-launcher gets from **ai-memory** (persistent sessions, MCP memory across
runs) has no v1 equivalent in Icaro — within a run, steps share `/workspace` and
outputs, which covers most pipelines. Cross-run agent memory is deliberately an open
question (§9) rather than an MVP feature: it could later be a mounted session-store
volume or an ai-memory MCP sidecar the harness connects to.

## 3. Triggers

Triggers are a **separate subsystem from steps** — they start runs, they don't run
inside them. Three types cover nearly everything:

1. **Webhook** — every workflow gets a URL: `POST /hooks/{workflow}/{token}`.
   The engine verifies an HMAC signature (per-trigger secret) and passes the payload
   as the run's initial input (`/icaro/input.json` of the first step). This single
   mechanism integrates GitHub, Stripe, Slack events, cron services — anything that
   can POST.
2. **Schedule** — cron expressions stored with the workflow; a scheduler loop enqueues runs.
3. **Manual / API** — `POST /api/workflows/{id}/runs` with an input payload (also what
   a CLI and UI use).

**Polling triggers** (e.g. "when a new row appears in X") should *not* be a special
trigger type in the MVP. Model them as a scheduled workflow whose first step checks
the source and exits with a "skip" output if nothing is new. This keeps the trigger
subsystem tiny; a dedicated polling-trigger abstraction can be added later if it hurts.

For most providers the trigger subsystem needs zero per-provider code — provider
specificity lives in payload handling inside steps. The one deliberate exception is
source-control providers, which get a thin adapter layer:

### 3.1 SCM provider triggers: GitHub App & GitLab

Being installable as a **GitHub App** (and the GitLab equivalent) is worth first-class
support, because SCM providers differ from plain webhooks in three ways: their own
signature schemes, an app/installation auth model, and high event volume that needs
filtering *before* a run is enqueued. The design: a narrow, compiled-in **trigger
adapter** interface —

```
verify(request) → ok        // provider signature check
normalize(request) → event  // {provider, type, repo, ref, actor, raw_payload}
```

— and nothing more. Routing, filtering, and enqueueing stay in the generic trigger
gateway; anything smarter than verify+normalize belongs in steps. Two adapters ship
built-in, and the interface is small enough that community adapters (Bitbucket,
Gitea) are trivial PRs.

**GitHub (as a GitHub App):**

- Icaro exposes one endpoint, `POST /hooks/github`, registered as the App's webhook
  URL. Signature check via `X-Hub-Signature-256` with the App's webhook secret.
- One App installation covers many repos and orgs; events for *all* installations
  arrive at the same endpoint, and the gateway routes them to workflows by filter.
- The App's credentials (app ID + private key) become a **connection type**
  (`github-app`). This is the payoff of app-mode: the same connection lets *steps*
  mint short-lived installation tokens to call back — post a comment, set a commit
  status, create a check run — so trigger and API access come from one install,
  with no personal access tokens involved.
- Registration should use GitHub's **App Manifest flow** (`POST /settings/apps/new`)
  so `icaro github-app setup` can create the App, capture the credentials, and store
  the connection in one guided step.

**GitLab:**

- GitLab has no App primitive; the equivalents are **project/group webhooks** (SaaS
  and self-managed) and **system hooks** (self-managed, instance-wide). Same single
  endpoint pattern: `POST /hooks/gitlab`, verified via the `X-Gitlab-Token` secret
  header (note: shared secret comparison, not HMAC — the adapter hides this
  difference).
- Callback auth is a `gitlab` connection type holding a group/project **access
  token** (or OAuth app credentials later). Setup can automate webhook creation via
  the GitLab API given a token — the same guided-setup UX as the GitHub App flow.

**Event filtering lives in the workflow trigger config**, evaluated by the gateway
before enqueueing (an agent- and human-friendly surface, and cheap — no container
spins up for filtered-out events):

```yaml
on:
  github:
    events: [pull_request]
    actions: [opened, synchronize]
    repos: [ggrocco/*]
    branches: [main]
```

The normalized envelope is deliberately minimal — provider, event type, repo, ref,
actor — with the **raw payload passed through untouched** as the run input. Steps get
full provider fidelity; the envelope exists only for filtering and run metadata.
Resist the temptation to build a rich cross-provider event model — that's the road
to maintaining a translation layer forever.

Also non-negotiable for SCM volume: **webhook delivery records** (persist every
received event with its verification result and matched workflows) and **redelivery**
from that record — debugging "why didn't my workflow fire?" is impossible without it,
and it doubles as the dedup point for providers that retry deliveries.

## 4. Proposed architecture

```
                 ┌──────────────────────────────┐
  webhooks ────► │  API / Control plane          │
  cron loop ───► │  - workflow & run CRUD        │──► DB (SQLite default;
  CLI / UI ────► │  - trigger gateway + SCM      │     Postgres via config)
  agents (MCP) ► │    adapters (GitHub/GitLab)   │     defs, runs, logs index,
                 │  - connection store*          │     connections*, deliveries
                 │  - MCP server (see §5)        │
                 └──────────────┬───────────────┘
                                │ enqueue run (DB-backed queue)
                 ┌──────────────▼───────────────┐
                 │  Runner (worker)              │
                 │  - resolves steps & manifests │──► Docker API
                 │  - injects inputs/connections │    (one container per step,
                 │  - collects outputs & logs    │     shared /workspace volume)
                 └──────────────────────────────┘
```

\* connection fields encrypted at rest (e.g. libsodium sealed box, key from env/KMS).

Implementation notes:

- **One binary, two roles.** Ship a single binary that can run as `server` and
  `runner` (or both, for single-machine setups). Go is the natural fit (Docker SDK,
  single static binary), but the shape works in any language.
- **Storage: SQLite by default, Postgres by configuration.** A single
  `database.driver: sqlite | postgres` + DSN config setting selects the backend.
  SQLite is the right default for a self-hosted, single-node tool — zero external
  dependencies, the whole state is one backup-able file — and Postgres is the
  scale-out path (multiple runners, HA) rather than a day-one requirement. What
  keeping both honest requires:
  - A thin repository interface with **portable SQL** — no Postgres-only features
    (`jsonb` operators, arrays, `SKIP LOCKED`); JSON payloads stored as TEXT and
    parsed in code. Migrations maintained per driver from day one; CI runs the full
    test suite against **both** backends, or the "swap by config" promise rots.
  - SQLite operational settings baked in, not left to the user: WAL mode,
    `busy_timeout`, a single writer connection (SQLite is single-writer by design —
    the engine must serialize writes through one pool connection to avoid
    `SQLITE_BUSY` surprises).
- **Queue:** DB-backed in the same storage layer, portable claim semantics: a
  transactional `UPDATE ... SET claimed_by WHERE id = (SELECT ... LIMIT 1)` works on
  both backends. On Postgres this can later be upgraded to `FOR UPDATE SKIP LOCKED`
  behind the same interface when multiple runners arrive. Do not introduce
  Redis/RabbitMQ until multiple runners exist and it hurts.
- **Workflow definition:** YAML, stored via API (optionally synced from git later).
- **DAG vs. sequence:** start with a linear sequence + `if`/`skip` conditions.
  A full DAG (`needs:`) is a clean later addition; don't pay its complexity up front.

### Example workflow

```yaml
name: notify-on-signup
on:
  webhook: {}                      # POST /hooks/notify-on-signup/<token>
steps:
  - name: enrich
    run:
      image: python:3.12-slim
      script: |
        python /workspace/scripts/enrich.py   # reads /icaro/input.json
  - name: notify
    uses: slack-message@1          # packaged integration
    connection: slack-prod
    inputs:
      channel: "#signups"
      text: "New signup: {{ steps.enrich.outputs.email }}"
```

## 5. Agent interface: MCP server + authoring skill

Agents are first-class users of Icaro, on equal footing with the CLI and a future UI.
Two deliverables make that real: an **MCP server** exposing the engine, and a
**skill** that teaches agents the authoring loop. The guiding principle: agents
hallucinate config fields; a published schema plus a validate tool turns workflow
authoring into a converging loop instead of guesswork.

### 5.1 Schema-first: one source of truth

The workflow definition gets a formal **JSON Schema**, generated at build time from
the engine's own types (e.g. Go structs → schema via `invopop/jsonschema`). The same
schema is used by:

1. the engine itself, to validate every workflow submitted via API;
2. the MCP server, published to agents;
3. docs/editors (YAML language servers understand JSON Schema for free).

Because it is generated, schema and behavior can never drift. The schema covers the
whole definition including trigger config — so an agent writing an `on.github`
filter block (§3.1) gets the same precision as for steps. Integration manifests
(§2.2) already declare their inputs — the server compiles each manifest into a schema
fragment, addressable as `icaro://schema/integrations/{name}@{version}`, so precision
extends into `uses:` steps: an agent can know that `slack-message@1` requires
`channel` and `text` before it writes a line of YAML.

### 5.2 The MCP server

Built into the same binary — a thin layer over the same service layer as the REST API
(never a parallel implementation). Transports: **stdio** (`icaro mcp`) for local use
and **streamable HTTP** (`/mcp` on the server) for remote agents; auth with the same
API tokens as the REST API.

Resources:

- `icaro://schema/workflow` — the workflow JSON Schema (also `get_workflow_schema`
  as a tool, since some clients handle tools better than resources).
- `icaro://schema/integrations/{name}@{version}` — per-integration input schemas.

Tools (the minimal, high-leverage set):

| Tool | Purpose |
|---|---|
| `get_workflow_schema` | Full JSON Schema for workflow definitions |
| `list_integrations` / `get_integration` | Catalog + manifest incl. input/output schema |
| `list_connections` | Names and **types only — never secret values** |
| `validate_workflow` | Validate a definition without saving; structured errors |
| `create_workflow` / `update_workflow` / `get_workflow` / `list_workflows` | CRUD |
| `run_workflow` | Start a run with an input payload (also serves as test/dry run) |
| `get_run` | Status, per-step outputs, log tail — enough to debug a failure |

Design notes:

- **`validate_workflow` is the most important tool.** Errors must be structured and
  precise — JSON Pointer path, expected vs. got, and the valid alternatives
  (`/steps/1/uses: unknown integration 'slak-message'; nearest match:
  'slack-message@1'`). Error quality here directly determines how fast an agent
  converges on a correct workflow.
- `get_run` should truncate logs (tail + size cap) so agents can debug without
  blowing their context window.
- Destructive operations (delete workflow, delete connection) are deliberately left
  out of the MCP surface in v1; agents author and run, humans prune.

### 5.3 The authoring skill

Ship an `icaro-workflows` skill (a `SKILL.md` package, installable into Claude Code
and other agents) in the main repo, versioned with the engine. It encodes the loop:

1. `get_workflow_schema` + `list_integrations` — **always re-fetch; never write from
   memory** (schemas change between engine versions).
2. `list_connections` to see what credentials exist (ask the human to create missing
   ones — agents never handle secret values).
3. Draft the workflow → `validate_workflow` → fix until clean.
4. `create_workflow`, then `run_workflow` with a small test input.
5. `get_run` to verify; iterate on failures using per-step outputs and logs.

The skill also carries the step-contract cheat sheet (§2.1), a couple of worked
examples, and known gotchas (e.g. "webhook payload becomes the first step's
`/icaro/input.json`"). Keeping it in-repo means every engine release that changes
the schema ships the matching skill update in the same commit.

### 5.4 In-step MCP: the engine exposed inside agent steps

The same MCP server, exposed *inside* agent step containers, lets a harness running
in a workflow call other workflows and create or change workflows mid-run. This is
the natural completion of §2.5 + §5: outside agents author workflows; inside agents
compose them. It should be in the blueprint — with the transport and the authority
model pinned down, because both are easy to get wrong.

**Transport: a Unix socket, not the network.** The runner bind-mounts a per-step
socket at `/icaro/mcp.sock` and the harness image's entrypoint wires it into the
harness's MCP client config automatically. This matters because it composes with the
sandbox (§6.1): a step with `network: none` can still reach the engine — and *only*
the engine; no TCP endpoint to discover, nothing reachable from other containers,
and the capability disappears when the mount does.

**Authority: per-step, minted, and visible in the schema.** The runner mints a
short-lived token scoped to the run and injects it behind the socket. What the token
allows is an explicit workflow field, consistent with "permission toggles become
schema fields" (§2.5):

```yaml
steps:
  - name: orchestrator
    agent: { harness: claude, prompt: ... }
    icaro_access: invoke     # none (default) | read | invoke | author
```

- `none` — no socket mounted. The default: most steps don't need the engine.
- `read` — schema, catalog, own-run status. Safe introspection.
- `invoke` — plus `run_workflow`/`get_run` on other workflows. The workhorse level.
- `author` — plus `create_workflow`/`update_workflow`, **landing as drafts**: a
  workflow written by an in-step agent is stored versioned and inert until a human
  (or an explicitly configured auto-approve rule) activates it. This is the
  prompt-injection firewall: an agent step processes untrusted trigger payloads, and
  a payload that talks the agent into rewriting automation must not yield a live
  workflow by itself. Actor identity (which run/step authored what) goes in the
  version history.

**Recursion needs brakes, not trust.** Workflows invoking workflows is a queue fork
bomb waiting for a retry loop: every run records `root_run_id` + `parent_run_id`
lineage; enforce a chain-depth cap (e.g. 5), a per-root-run budget of descendant
runs, and reject self-invocation cycles at `run_workflow` time. Lineage doubles as
observability — "what did this webhook ultimately cause" is one query.

**Deterministic composition stays out of the agent.** When a workflow always calls
another workflow, that belongs in a `call_workflow` built-in step (sub-workflow as a
step, outputs mapped back), not in an agent's judgment. In-step MCP `invoke` is for
the cases where *deciding what to run* is the agent's job. Offer both; the skill
(§5.3) should say when to use which.

## 6. Security considerations (must be in the blueprint, not an afterthought)

- **The runner owns the Docker socket; step containers must never see it.** Run step
  containers with no docker.sock mount, a non-root user where possible, memory/CPU
  limits, and a hard timeout per step. §6.1 hardens this further.
- **Webhook endpoints are internet-facing:** HMAC verification, per-trigger tokens,
  payload size limits, and rate limiting from day one.
- **Secrets:** never write connection values to logs or `/workspace`; scrub known
  secret values from captured logs (Drone does this — copy it).
- **Output limits:** cap `/icaro/output.json` size (e.g. 1 MB) to protect the DB.
- **MCP surface:** same authentication as the REST API, secrets never readable
  through any tool, and no destructive tools exposed to agents in v1 (§5.2). In-step
  access is a separate, narrower authority: per-run minted tokens, `icaro_access`
  levels, agent-authored workflows land as drafts (§5.4).

### 6.1 Step sandboxing — ai-jail concepts applied to Docker

Icaro will run AI agents *inside steps*, and agent-generated commands must be treated
as untrusted code. The scenario to design against: a step (or a prompt-injected agent
in a step) escalating out of its container — with the Docker socket as the classic
escape route to root on the host. [ai-jail](https://github.com/akitaonrails/ai-jail)
is the right reference for the mindset. Note it is *not* Docker-based (it sandboxes
with bubblewrap, Landlock, and seccomp directly on the host OS), so Icaro adopts its
**concepts**, translated to the container runtime, in three layers:

**Layer 1 — the socket never crosses the boundary (already §6, restated as policy):**
the runner talks to Docker; steps get no docker.sock bind-mount ever, and the engine
should *refuse* a workflow whose `run.volumes` tries to mount it (or anything under
`/var/run`, `/proc`, `/sys`) — validation-time rejection, not just convention. On the
runner side, reduce the blast radius of the socket the runner itself holds: run
**rootless Docker** (or Podman), or front the socket with a **docker-socket-proxy**
that allowlists only the API calls the runner needs (create/start/wait/logs/remove) —
so even a compromised runner cannot `docker exec` into neighbors or mount host paths.

**Layer 2 — hardened defaults for every step container** (ai-jail's philosophy of
deny-by-default resource and syscall access, expressed as Docker flags):

| ai-jail concept | Icaro equivalent |
|---|---|
| Seccomp-BPF blocking ~30 dangerous syscalls | Custom seccomp profile: Docker's default *minus* nothing, *plus* deny `ptrace`, `bpf`, `mount`, `keyctl`, module & namespace syscalls |
| No privilege escalation | `--security-opt no-new-privileges`, `--cap-drop ALL` (add back only what a step declares) |
| RLIMIT_NPROC / fork-bomb protection | `--pids-limit` (e.g. 256), memory + CPU limits, `--ulimit nofile` |
| tmpfs `$HOME`, mask `.env`/`.ssh`/`.aws` | Read-only root FS (`--read-only`) + tmpfs `/tmp` and `/home`; only `/workspace` and `/icaro` are real mounts |
| Selective project mounts | Steps see *only* the run's workspace volume — never host paths; host bind-mounts are not a feature |
| User separation | `--userns-remap` (or rootless engine) so root-in-container ≠ root-on-host |

**Layer 3 — a `sandbox` profile in the workflow schema**, so strictness is explicit,
validated, and visible to agents via the published JSON Schema (§5.1):

```yaml
steps:
  - name: agent-task
    run:
      image: my-agent:latest
      script: ...
    sandbox: strict     # default: standard
    network: none       # default: egress; 'none' for pure-compute steps
```

- `standard` — Layer-2 defaults above; suitable for trusted scripts and packaged
  integrations.
- `strict` — additionally `network: none` unless declared, tighter pids/ulimits, and
  (when installed) a **gVisor (`runsc`) or Kata runtime** for kernel-level isolation.
  This is the profile the docs recommend for steps that run LLM-driven agents, and
  ai-jail's own caveat applies: process/container sandboxes are "not 100% secure, but
  enough" — for truly hostile workloads the answer is a VM-isolated runtime, which is
  exactly what the pluggable `runtime` escape hatch is for.

Defaults matter more than options: `standard` is applied with **zero configuration**,
and nothing in the schema allows weakening below it (no `privileged: true`, ever).

## 7. What to borrow, what to avoid

| System | Borrow | Avoid |
|---|---|---|
| Drone CI | Env-var plugin contract; secret scrubbing | CI-specific YAML semantics |
| GitHub Actions | `uses: name@version` + manifest (`action.yml`) idea | JS runtime actions, marketplace complexity |
| n8n | Connection/credential UX | Per-node SDK + bespoke UI code per integration |
| Argo Workflows | Container-native step model | Kubernetes dependency for an MVP |
| Temporal | Nothing for MVP | Whole programming model — overkill here |
| ai-jail | Deny-by-default sandbox mindset: seccomp, rlimits, tmpfs HOME, secret masking (§6.1) | Its mechanism as-is — bubblewrap targets host processes, not containers |
| ai-launcher | Harness catalog with declared params, digest/checksum pinning, opt-in capabilities, dry-run UX (§2.5) | TUI/PTY interactivity, host mounts, desktop toggles — no server equivalent |

## 8. Suggested build order

1. **Engine core** — workflow YAML validated against a generated JSON Schema from day
   one (§5.1), linear steps, Docker runner with the hardened `standard` sandbox as
   the only mode (§6.1), step contract (inputs/outputs/workspace/logs), SQLite
   storage behind the driver-swappable repository layer (§4) with both-backend
   tests from the first migration, manual trigger via API + CLI. *Usable on day one.*
2. **Triggers** — generic webhook gateway with HMAC + cron scheduler, including
   webhook delivery records from the start.
3. **Connections** — encrypted store + injection + `http` built-in step.
4. **Agent interface** — MCP server (schema resource, `validate_workflow`, CRUD,
   runs) + the `icaro-workflows` authoring skill.
5. **SCM triggers** — the adapter interface + GitHub App adapter (manifest-flow
   setup, `github-app` connection type) + GitLab adapter, with event filtering
   and redelivery.
6. **Integration manifests + harness catalog** — `uses:` resolution, git-repo
   catalog, input validation compiled into per-integration schemas exposed over MCP;
   the `agent` step type and first harness images (§2.5) ride on the same machinery,
   then in-step MCP over the per-step socket with `icaro_access` levels and the
   `call_workflow` built-in step (§5.4).
7. **Later, by demand** — DAG execution, approval steps, polling trigger sugar,
   step presets, cross-run agent memory, UI, multi-runner scale-out (which is the
   moment the Postgres config swap earns its keep).

Steps 1–3 already deliver the stated goal ("run scripts on Docker, triggered by and
talking to the outside world"). Step 4 is cheap if step 1 was schema-first — the MCP
server mostly re-exposes existing service methods. Step 5 is what makes integrations
*cheap forever*, and it plugs straight into the MCP schema surface.

## 9. Open questions for the author

- Single-tenant self-hosted tool, or multi-user with auth from the start?
  (Recommendation: single-tenant first; auth is a big detour.)
- Expected step duration — seconds or hours? Affects timeout defaults and whether
  runs must survive a runner restart (recommendation: persist step state so they do).
- Is a UI in scope for v1, or is CLI + YAML enough? (Recommendation: CLI first;
  the manifest design already leaves room for form-rendering later.)
- Cross-run agent memory (§2.5): do harnesses need persistent context between runs —
  and if so, via a session-store volume, an ai-memory MCP sidecar, or nothing?
  (Recommendation: defer; within-run `/workspace` sharing covers most pipelines.)
