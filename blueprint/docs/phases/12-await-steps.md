# Phase 12 — Await steps

**Objective:** a workflow step that parks the run until a matching integration event arrives, as proposed in [ADR 0032](../adr/0032-await-event-steps.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs ADR 0032 **accepted**, with its open questions settled: whether `key`/`match` stay required, where the lookback starts, and whether validation warns when `daemon.public_url` is unset. A change to any of them is made in the ADR first, then here.

## Deliverables

- [ ] **Contract.** Add the `await` step form (`integration`, `event`, `key`, `match`, `filter`) to the spec types in `internal/workflow`, regenerate the schema, and update the [blueprint contract](../schemas/workflow.schema.json) so the drift test passes. A step has exactly one of `image`, `uses`, `approval`, `wait` or `await`.
- [ ] **Validation.** Resolve `integration`/`event` through the catalog, with nearest-match hints. Check that `key` uses only `trigger`, `env`, `run`, `workflow` and `needs`-ancestor outputs and no `secrets`. Check that `match` and `filter` parse at validation time and reference only payload fields the event declares, under the root `event`. Require `timeout` (at most `720h`, exempt from `limits.timeout`). Refuse `command`, `env`, `input`, `outputs`, `repository` and `retry`. Later steps may reference the event's declared fields and `delivery_id` as outputs.
- [ ] **Storage.** Per-dialect migrations for `delivery_events` and `step_awaits`. `delivery_events` is pruned with its delivery under `webhooks.retention`. Store methods change `step_awaits` state only by compare-and-set from `waiting`, and set `runs.wake_at` in the same transaction.
- [ ] **Pipeline.** In `internal/integration`, persist each matched event's normalized payload before reading waiting rows. Then add step 6b: evaluate each waiting step's `match` and `filter` and claim the first event that correlates. Generic `webhook` events correlate only with runs of the workflow whose token received them. Delivery records list the steps a delivery resumed. A failed write records the delivery as `error` and answers 503. Redelivery runs 6b too.
- [ ] **Engine.** In `internal/workflow/engine`: start an `await` step by rendering `key` (an empty key fails the step), committing the `waiting` row, then looking back through `delivery_events` from the run's start, oldest first. Park and wake as for the other pause steps ([ADR 0030](../adr/0030-approval-wait-steps.md)). Record `expired` past the deadline (failing the step and skipping its dependents), and `cancelled` on run cancel, `replace` or workflow delete. On resume, write the matched payload to the step's outputs.
- [ ] **Resolving by hand.** `POST /api/v1/runs/{id}/steps/{step}/deliver` (`write` scope; payload checked against the event's declared fields; actor `token:<name>`, `delivery_id` `manual:<id>`), `icaro runs deliver RUN STEP --payload FILE`, and a form on the web UI's run page. No MCP tool for it. `get_run` and `icaro runs get` show the integration, event, rendered key, deadline and state of each `await` step.
- [ ] **Clients.** The web UI's step views, the run graph and the desktop tray know the `await` kind, and list runs parked on it among the waiting ones.
- [ ] **Laptop warning**, if ADR 0032 keeps it: validation warns, without failing, when a workflow awaits an integration event and `daemon.public_url` is unset. `icaro doctor` reports the same.
- [ ] **Docs.**
  - `docs/workflows.md`: an "Await steps" section with a worked example, checked by `hack/docscheck`.
  - `docs/integrations.md`: delivery outcomes now list resumed steps.
  - [Architecture §6](../01-architecture.md#6-workflow-engine) and §9.
  - The blueprint: ADR 0032 marked Accepted, a decision-index row, cut 8's wording, and the deferred-designs row removed.

## Acceptance criteria

- [ ] An invalid `await` step returns structured issues with a JSON Pointer path for each case: an unknown integration or event (with a nearest-match hint), a missing or over-long `timeout`, `secrets` in `key`, a reference to a step that is not a `needs` ancestor, and a `match`/`filter` field the event does not declare.
- [ ] Two concurrent runs of one workflow, awaiting the same event type with different keys, are each resumed only by the event carrying their own key.
- [ ] An event received after the run started but before the `await` step started (its `needs` ancestor still running) resumes the step as soon as it starts.
- [ ] An event delivered while the step is starting, between the commit of the `waiting` row and the lookback, is used exactly once. A test makes both orders happen deterministically.
- [ ] An event received before the run started does not resume it, and neither does one whose `filter` is false; the step keeps waiting.
- [ ] A duplicate delivery resumes nothing. An event arriving after the step matched, expired or was cancelled is ignored, and its delivery record shows that it resumed nothing.
- [ ] A step past its deadline fails with `no matching <integration>.<event> before <deadline>`, and its dependents are skipped. This holds when the daemon was stopped across the deadline.
- [ ] A run parked on an `await` step holds no `workflows.max_runs` slot and no container. After a daemon restart it still resumes on its event.
- [ ] An event delivered on another workflow's generic-webhook token does not resume the run.
- [ ] When the database write in step 6b fails, the delivery is answered with 503. The provider's retry then resumes the step exactly once.
- [ ] Resolving by hand: a payload with an undeclared field is refused, the `read` scope is refused, the actor is recorded, and no MCP tool can do it.
- [ ] A delivery checked against 1,000 waiting steps is handled within a bound measured on the reference machine and added to the budget table of [ADR 0024](../adr/0024-hardening-budget.md); the small size runs under `-race`, the full size under `-tags load`.
- [ ] Every new store and engine decision has unit tests against the fake engine. The resume-after-restart and real-webhook paths have tests behind the `integration` build tag. Coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets.
