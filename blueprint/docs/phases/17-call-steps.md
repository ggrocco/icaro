# Phase 17 — Call steps

**Objective:** a `call` step that starts another workflow as a child run, parks the caller until the child finishes, and returns the child's workflow outputs, with lineage and brakes against runaway recursion, as proposed in [ADR 0038](../adr/0038-call-workflow-steps.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs ADR 0038 **accepted**, with its open questions settled:

- the default brakes (`max_call_depth` and `max_runs_per_root`);
- whether `replace` on a called workflow acts as `queue` or is refused;
- whether the parent shows the child's logs inline or links to them;
- whether the step `timeout` is optional or required.

A change to any of them is made in the ADR first, then here.

## Deliverables

- [ ] **Contract.** In the spec types of `internal/workflow`, add:
  - the `call` step form (`workflow`, `input`);
  - the `call` built-in trigger type;
  - workflow-level `outputs`;
  - `call` as a run trigger kind.

  Regenerate `schema/workflow.schema.json` with `go generate ./...`, and update the [blueprint contract](../schemas/workflow.schema.json) so the drift test passes.
- [ ] **Validation.** Through a workflow catalog interface (nil skips the cross-workflow checks, as the integration catalog does), validation checks the following, each with a JSON Pointer and a nearest-match hint where one exists:
  - `workflow` is a literal name of an existing workflow with a `call` trigger;
  - `input` covers exactly the `trigger.*` fields the child references, the same analysis `icaro run` uses to prompt;
  - `input` values use only `steps` (of `needs` ancestors), `trigger`, `env`, `run` and `workflow`, with no `secrets`;
  - `${{ steps.<call>.outputs.<key> }}` names one of the child's declared outputs;
  - workflow `outputs` reference only real step outputs;
  - `command`, `image`, `env`, `outputs` and `repository` on a `call` step are errors.
- [ ] **Call graph.** Saving a workflow walks its `call` steps through the current versions of the workflows they call. A cycle is refused, with the path in the issue (`a → b → a`). Validating a callable workflow also validates its callers, and the issues name the caller.
- [ ] **Config.** `workflows.max_call_depth` and `workflows.max_runs_per_root`, with the defaults the ADR settles, validated at load.
- [ ] **Store.** A migration in both dialects adds the child run id and the child's workflow version to step runs, plus an index on `runs(parent_run_id)` and on `runs(root_run_id)`. The engine starts filling the existing `root_run_id`, `parent_run_id` and `depth` columns. Tests pass on SQLite and Postgres.
- [ ] **Engine.** A new `internal/workflow/engine/call.go`:
  - **Start.** Check the depth cap and the per-root budget, then enqueue the child in one transaction with the step's bookkeeping. The child gets `trigger_kind: call`, the lineage columns, actor `run:<parent>`, idempotency key `call:<parent run>:<step>:<attempt>`, and the recorded child version (the original's on a re-run).
  - **Wait.** Park the caller as for approvals ([ADR 0030](../adr/0030-approval-wait-steps.md)). It holds no slot and no container, and `wake_at` is set by the step's `timeout` when it has one.
  - **Wake.** The transaction that records a child's final state also sets its parent's `wake_at` to now.
  - **Outcome.** Map the child's state to the step as the ADR's table does. A timeout cancels the child and fails the step. `retry` starts a new child per attempt.
  - **Concurrency.** The child's policy applies, with `replace` handled as the ADR settles.
- [ ] **Cancellation.** Cancelling a run, `replace` on its workflow, or deleting it cancels its active descendants through `parent_run_id` before the run is recorded `cancelled`. Cancelling a child by hand fails only the parent's step.
- [ ] **Re-runs.** A re-run of a parent ([ADR 0013](../adr/0013-workflow-execution-semantics.md)) calls the child versions the original run recorded, and is a new root.
- [ ] **Clients.**
  - `get_run`, `icaro runs get` and the API show a `call` step's child run (id, status, version), and on any run its parent and root.
  - `icaro runs list --root <id>` lists a tree; `go run ./hack/gendocs` regenerates `docs/cli.md`.
  - The web UI's run page links parent and child, the run graph draws a `call` step as a box that opens the child's graph, and a workflow page lists its callers.
  - The form editor offers the `call` form, with the callable workflows and their required inputs.
- [ ] **Skill.** The `icaro-workflows` skill learns `call`: find a callable workflow before copying its steps, and declare outputs on workflows meant to be called.
- [ ] **Docs.**
  - `docs/workflows.md`: a "Calling workflows" section with a caller and a callee in ```` ```yaml workflow ```` blocks (checked by `hack/docscheck`), plus the brakes and the concurrency rule.
  - `docs/cli.md`, regenerated.
  - [Architecture §6](../01-architecture.md#6-workflow-engine) and §10.
  - The blueprint: ADR 0038 marked Accepted, a decision-index row, cut 8's wording, and the sub-workflow half removed from the deferred-designs row.

## Acceptance criteria

- [ ] A caller and a callee from the docs run end to end: the caller's later step reads the callee's workflow output, and the run page shows both runs linked.
- [ ] Invalid uses return structured issues with JSON Pointer paths:
  - a missing callee, or one without a `call` trigger (with a hint);
  - a missing or extra input;
  - `secrets` in an input;
  - an undeclared child output;
  - a forbidden field on the step.
- [ ] Saving `a` that calls `b` that calls `a` is refused, with the path. Editing `b` later so that it calls `a` makes `b`'s save fail. A cycle that slips through, through a version saved before the check existed, stops at the depth cap.
- [ ] With `workflows.max_runs: 1`, a three-deep chain of calls completes. Parked callers free the slot, so nothing deadlocks.
- [ ] A call past `max_call_depth`, or past `max_runs_per_root` (counting retries), fails the step with the limit's name and starts nothing.
- [ ] A daemon killed between the child's enqueue and the step's bookkeeping restarts with exactly one child run, and the parent still wakes when it finishes.
- [ ] The parent wakes within one engine poll of the child's final state, after a daemon restart too.
- [ ] Cancelling the root cancels every active descendant before the root is recorded `cancelled`. Cancelling a child by hand fails the parent's step and leaves the parent running its other branches.
- [ ] A step `timeout` cancels the child and fails the step. A child parked on an approval keeps its parent parked, without a slot, until the approval is decided.
- [ ] A callee with `concurrency: forbid` fails a second concurrent call with the policy's reason. A callee with `replace` never cancels a run another parent is waiting on.
- [ ] A re-run of a parent calls the child versions the original recorded, even after the child was edited.
- [ ] Every new decision has unit tests against the fake engine. Lineage, cancellation down the tree and restart recovery have tests behind the `integration` build tag. Store changes pass on SQLite and Postgres, and coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets.

## Where it lives

- Spec, schema, validation and the call graph: `internal/workflow`; execution: `internal/workflow/engine/call.go`.
- Lineage, indexes and the step-run columns: `internal/store`; cancellation down the tree: `internal/service` and the engine.
- Config: `internal/config`; API, CLI and UI: `internal/api`, `internal/cli`, `web/src/pages`.
