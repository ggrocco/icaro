# ADR 0032 — Steps that wait for an integration event

**Status:** Proposed. Would lift [MVP cut 8](../02-decisions-risks.md#mandatory-mvp-cuts) for one more step form, the last pause listed under [deferred designs](../02-decisions-risks.md#deferred-designs). Builds on [ADR 0030](0030-approval-wait-steps.md) (parking) and [ADR 0015](0015-manifest-schema-webhook-pipeline.md) (the webhook pipeline).

## Context

ADR 0030 added `approval` and `wait`, and left out the third pause users ask for: stop until something happens elsewhere. A workflow that pushes a branch should continue when CI on that commit finishes; one that deploys should continue when the deploy reports back. Today the only options are a `wait` that guesses how long it takes, or an image step that polls an API and holds a `workflows.max_runs` slot and a container while it does.

The events already arrive. Integration manifests verify and normalize them, and the pipeline records every delivery. What was missing, per the deferred-design note, is two rules:

- **Correlation:** how an event is tied to the one run that is waiting for it, when many runs of the same workflow wait for the same kind of event at once (the wedge is parallel sessions, [ADR 0002](0002-session-spine.md)).
- **Stray and late events:** what happens to an event that arrives before the step starts waiting, after it stopped, or for no waiting step at all.

## Decision

### The step form

**`await`**, a fifth step form. A step has exactly one of `image`, `uses`, `approval`, `wait` or `await`.

```yaml
- id: ci
  needs: [push]
  await:
    integration: github
    event: check_suite.completed
    key: ${{ steps.push.outputs.sha }}
    match: event.head_sha
    filter: event.conclusion != 'neutral'
  timeout: 2h
```

- `integration` and `event` name an event declared by a loaded manifest, as an integration trigger does. Validation rejects unknown ones with the same nearest-match hints.
- `key` is a template rendered **once, when the step starts**. It may use `trigger`, `env`, `run`, `workflow` and the outputs of the step's `needs` ancestors, but not `secrets`, because the rendered key is stored and shown in `get_run`, the CLI and the web UI. A key that renders empty fails the step.
- `match` is an expression over the incoming event's normalized payload, under the root `event`. It is evaluated for each candidate event; the event correlates when its result, as a string, equals the rendered `key`.
- `filter` (optional) is a boolean expression over `event`, evaluated after `match`. A correlated event that fails the filter is ignored and the step keeps waiting. This is how "the CI run for my commit, but only once it has a conclusion" is written.
- `match` and `filter` may reference only payload fields the event declares; validation checks them as it checks trigger filters.
- `timeout` is **required**: at most `720h` (the `wait` ceiling), and exempt from `limits.timeout`, as an approval deadline is. An event wait without a deadline would park a run forever on a typo in `key`.
- `command`, `env`, `input`, `outputs`, `repository` and `retry` are not allowed, as on the other pause steps.

**`key` and `match` are required.** A step that took the next `check_suite.completed` of any kind would resume on whichever run's CI finished first. A workflow that does want "any" writes a constant on both sides, so the choice is visible in the file.

**Outputs.** A matched step outputs every payload field the event declares, rendered as strings with the same conversion `${{ }}` uses, plus `delivery_id` (or `manual:<id>`, see below). Later steps read them as `${{ steps.ci.outputs.conclusion }}`. The payload is as untrusted as `trigger.*` is in an integration-triggered run and gets the same treatment: it is data passed to steps, never evaluated.

### Correlation and the race with the step's start

Two new tables:

- `delivery_events`: one row per event a verified delivery matched, holding its integration, event, normalized payload, the delivery's workflow (set only for the generic `webhook` integration's per-workflow token) and the received time. The pipeline writes it in step 5 of ADR 0015, before anything is enqueued or resumed. It is pruned with its delivery under `webhooks.retention` (`720h` by default, which is also the longest timeout an `await` step can have; a shorter retention shortens the lookback, not the deadline).
- `step_awaits`: one row per `await` step of a run: integration, event, the rendered key, the deadline, a state (`waiting`, `matched`, `expired`, `cancelled`), and, once decided, the delivery or manual id, the payload and the time. A state change is a compare-and-set from `waiting`, so the first event to arrive wins and the decision is final.

**On each delivery**, a new pipeline step 6b runs after workflows are matched: for every `waiting` row on the same integration and event, evaluate the step's `match` (and `filter`) over each of the delivery's events, and claim the first that correlates. The claim sets `runs.wake_at` to now in the same transaction, as an approval decision does. One delivery can resume any number of waiting steps, and can both start new runs and resume old ones; each is listed in the delivery record, which is `processed` if it did either.

**When the step starts**, the engine first commits the `waiting` row, then looks back through `delivery_events` for the same integration and event received since the **run** started, oldest first, and claims the first that correlates. The order is what closes the race: the pipeline persists an event before it reads the waiting rows, and the engine persists the waiting row before it reads the events, so an event that lands while the step is starting is seen by one side or the other, and the compare-and-set makes sure it is used once.

The lookback starts at the run, not the step, because the thing a step waits for is often caused by an earlier step (`push`), and its event can arrive before `push` finishes. It never reaches back before the run, so an event from a previous run with the same key is not taken.

**Generic webhooks.** For `integration: webhook`, only deliveries made on the run's own workflow's URL token correlate. That token is the credential ([ADR 0015](0015-manifest-schema-webhook-pipeline.md)); without this rule, anyone holding another workflow's token could resume this one.

### Stray and late events

- **Before the step waits:** covered by the lookback, from the start of the run.
- **Before the run starts:** ignored. A workflow that needs it should be triggered by that event instead.
- **After the step is decided** (matched, expired or cancelled): ignored for that step. The delivery record shows it resumed nothing.
- **No waiting step and no subscribed workflow:** the delivery is `no_match`, as today.
- **Duplicate deliveries** are dropped by the existing dedupe before step 6b. **Redelivery** of a stored record re-runs 5–7 and 6b, so it can resume a waiting step; that is deliberate, and useful when testing.

### Deadlines, parking and failure

Parking is exactly ADR 0030's: an `await` step holds no slot and no container, the run is `waiting` while only pause steps are left, and `wake_at` is the earliest deadline among them. Past its deadline, a `waiting` row is recorded `expired` by the engine and the step fails with `no matching <integration>.<event> before <deadline>`; the steps that need it are skipped. Cancelling the run, `replace`, or deleting the workflow records it `cancelled`.

If step 6b cannot write (the database is unavailable), the delivery is recorded as `error` and answered with 503, so the provider retries and dedupe keeps the retry safe, as for enqueueing.

### Resolving by hand

Inbound webhooks need a reachable daemon (cut 9), so on a laptop an `await` step may never see its event, and every integration-triggered workflow must stay testable by hand (ADR 0015). A person can therefore resolve a waiting step with a payload:

- `POST /api/v1/runs/{id}/steps/{step}/deliver`, body `{"payload": {...}}`, with the `write` scope. The payload is checked against the event's declared fields; `key`, `match` and `filter` are not applied, because the person picked the step. The actor is recorded as `token:<name>` and `delivery_id` as `manual:<id>`.
- `icaro runs deliver RUN STEP --payload FILE` and a form on the run page in the web UI call it.
- As with approvals, **no MCP tool resolves an `await` step**, and step containers cannot reach the daemon API, so a run cannot release itself.

## Alternatives considered

- **Poll from an image step.** Works today, but holds a run slot and a container for hours, and every workflow re-implements the same API loop.
- **One boolean `match` over both the run and the event**, with no rendered key. More flexible, but the expression would have to be evaluated against every waiting step's full run context on every delivery, and nothing could be indexed or shown. A rendered key is a value a person can read in the UI and compare with the event.
- **Buffer unmatched events in memory** instead of looking back through stored ones. Lost on restart, and the pipeline already persists every verified delivery.
- **Correlation declared in the manifest** (each event names its key field). Simpler to write, but one event has several useful keys (a check suite has a commit, a branch and a pull request), and the step is where the choice belongs.

## Consequences

- A workflow can wait for CI, a deploy or a review with no polling, no container and no slot. Waiting costs one row per step.
- Correlation is visible: the run page shows the rendered key, and each delivery lists the steps it resumed or why it resumed none.
- Every verified delivery now also stores its normalized events, which grows the database within the existing retention window.
- A manifest edit that removes an event or a payload field fails validation of the workflows that await it, as it already does for triggers. Steps already waiting keep their row and expire on their deadline if the event no longer arrives.
- `await` is a new step kind for API clients and the generated schema (with a drift test against [the contract](../schemas/workflow.schema.json)), the CLI and the web UI's step views.
- On a daemon no provider can reach, `await` steps only finish by hand or by expiring. Validation should warn, not fail, when a workflow awaits an integration event and `daemon.public_url` is not set.
- Implementation is [Phase 12](../phases/12-await-steps.md), after Phase 11, since every existing phase is closed.
