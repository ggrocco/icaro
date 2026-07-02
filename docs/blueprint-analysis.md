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

This means the trigger subsystem never needs per-provider code either — provider
specificity lives in payload handling inside steps, which are already easy to write.

## 4. Proposed architecture

```
                 ┌──────────────────────────────┐
  webhooks ────► │  API / Control plane          │
  cron loop ───► │  - workflow & run CRUD        │──► Postgres (defs, runs,
  CLI / UI ────► │  - trigger gateway (HMAC)     │     logs index, connections*)
                 │  - connection store*          │
                 └──────────────┬───────────────┘
                                │ enqueue run (Postgres queue is fine at first)
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
- **Queue:** start with a Postgres-backed queue (`SELECT ... FOR UPDATE SKIP LOCKED`).
  Do not introduce Redis/RabbitMQ until multiple runners exist and it hurts.
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

## 5. Security considerations (must be in the blueprint, not an afterthought)

- **The runner owns the Docker socket; step containers must never see it.** Run step
  containers with no docker.sock mount, a non-root user where possible, memory/CPU
  limits, and a hard timeout per step.
- **Network egress:** steps need outbound network (that's the point), but consider a
  per-workflow allowlist later; at minimum document that steps are trusted code.
- **Webhook endpoints are internet-facing:** HMAC verification, per-trigger tokens,
  payload size limits, and rate limiting from day one.
- **Secrets:** never write connection values to logs or `/workspace`; scrub known
  secret values from captured logs (Drone does this — copy it).
- **Output limits:** cap `/icaro/output.json` size (e.g. 1 MB) to protect the DB.

## 6. What to borrow, what to avoid

| System | Borrow | Avoid |
|---|---|---|
| Drone CI | Env-var plugin contract; secret scrubbing | CI-specific YAML semantics |
| GitHub Actions | `uses: name@version` + manifest (`action.yml`) idea | JS runtime actions, marketplace complexity |
| n8n | Connection/credential UX | Per-node SDK + bespoke UI code per integration |
| Argo Workflows | Container-native step model | Kubernetes dependency for an MVP |
| Temporal | Nothing for MVP | Whole programming model — overkill here |

## 7. Suggested build order

1. **Engine core** — workflow YAML, linear steps, Docker runner, step contract
   (inputs/outputs/workspace/logs), manual trigger via API + CLI. *Usable on day one.*
2. **Triggers** — webhook gateway with HMAC + cron scheduler.
3. **Connections** — encrypted store + injection + `http` built-in step.
4. **Integration manifests** — `uses:` resolution, git-repo catalog, input validation.
5. **Later, by demand** — DAG execution, approval steps, polling trigger sugar, UI,
   multi-runner scale-out.

Steps 1–3 already deliver the stated goal ("run scripts on Docker, triggered by and
talking to the outside world"). Step 4 is what makes integrations *cheap forever*.

## 8. Open questions for the author

- Single-tenant self-hosted tool, or multi-user with auth from the start?
  (Recommendation: single-tenant first; auth is a big detour.)
- Expected step duration — seconds or hours? Affects timeout defaults and whether
  runs must survive a runner restart (recommendation: persist step state so they do).
- Is a UI in scope for v1, or is CLI + YAML enough? (Recommendation: CLI first;
  the manifest design already leaves room for form-rendering later.)
