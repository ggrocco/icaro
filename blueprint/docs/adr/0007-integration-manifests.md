# ADR 0007 — Integrations are declarative manifests in git-backed packs

**Status:** Accepted

## Context

The original integration story had three gaps. Trigger types were a closed enum
(`github`, `gitlab`, `webhook`) baked into Go code, so every new source (Jira,
Linear, Sentry, Datadog) meant a schema change and a release. Trigger event data
never reached the steps — there was no defined way for a step to read the PR
number or branch that fired the run. And calling an external API required
authoring, publishing, and pulling a container image, which makes the simplest
integration ("POST to Slack") disproportionately heavy. The architecture also
carried a contradiction: `internal/notify/` existed as a built-in package while
the decisions doc declared notifications "just an image".

The target integrations for the first releases are GitHub, GitLab, Slack, Jira,
Linear, Sentry, and Datadog — inbound events, outbound actions, or both.
Inbound webhooks assume a **reachable daemon** (homelab/VPS with a hostname);
polling is the documented laptop fallback, designed for but not built in the MVP.

## Decision

An integration is a **declarative YAML manifest** — no code — and manifests are
distributed in **packs, which are git repositories**.

- A manifest declares **triggers** (webhook signature verification, event
  matching, and normalization of the raw payload into a typed `${{ trigger.* }}`
  context) and **actions** (typed inputs, an HTTP request template, auth by
  reference, and output extraction from the response). One interpolation
  language is shared with workflows.
- The engine grows exactly three generic pieces: one webhook endpoint per
  integration (`/hooks/<name>`), one in-daemon HTTP action executor for `uses:`
  steps, and the `trigger.*` interpolation context. Everything else is data.
- **Packs are git repositories** under `~/.icaro/integrations/`. The seven
  first-party integrations ship as a read-only `builtin` pack embedded in the
  binary; `icaro integration add <git-url>` clones a third-party pack; the
  `local` pack is auto-initialized as a git repo on first edit, and **UI edits
  commit directly to it**. Push is manual by default. A user pack may
  deliberately shadow a builtin; a collision between two user packs is a
  validation error. The daemon watches packs and keeps the last good version of
  an invalid manifest loaded.
- Workflow steps take exactly one of `image` (the general compute primitive,
  unchanged) or `uses: <integration>.<action>` (an in-process HTTP action — no
  container). `uses:` steps are full DAG citizens: `needs`, `when`, `retry`,
  `timeout`, and `outputs` work identically.
- **Security:** integration credentials live in the daemon's secret store and
  are **host-bound at setup time** — the executor refuses to attach a secret to
  a request whose host is outside the binding the user confirmed, so a malicious
  pack update cannot redirect a token. Installing a pack shows a complete
  permission summary (hosts, auth kinds) — complete because manifests execute no
  code. The HTTP executor blocks private-range URLs by default (SSRF), with
  per-integration user opt-in for self-hosted services. Unsigned webhooks
  require a per-workflow random URL token; redaction covers response bodies.

## Consequences

- Adding integration #8 is a YAML file in a git repo, not a release. First-party
  and third-party integrations use the same mechanism, which keeps the seam honest.
- `internal/notify/` and the hardcoded webhook adapters disappear;
  `internal/integration/` becomes the manifest hub (packs, webhook router, HTTP
  executor). Phase 3 deliverables are replaced by the generic path plus manifests.
- The "step = image" purity is relaxed: `uses:` is a second, declarative step
  form. Anything a template can't express falls back to an `image` step — the
  escape hatch already exists.
- Workflows never touch raw webhook bodies; they consume the normalized
  `trigger.*` payload, so swapping GitHub for GitLab barely changes a workflow.
  Manual runs prompt for the payload fields, keeping every workflow testable by hand.
- Deliberately out of v1: OAuth flows (token/PAT first), pagination, polling
  triggers, and any scripting hooks in manifests.
