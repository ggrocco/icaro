# ADR 0038 — Sub-workflow steps: `call` runs another workflow and waits for it

**Status:** Proposed. Would lift [MVP cut 8](../02-decisions-risks.md#mandatory-mvp-cuts) for one more step form, `call`. Takes the sub-workflow half of the "`call_workflow` and in-container MCP access" row out of the [deferred designs](../02-decisions-risks.md#deferred-designs); in-container MCP stays deferred, apart from what [ADR 0035](0035-workflow-assistant-chat.md) proposes for assistant sessions. Builds on [ADR 0013](0013-workflow-execution-semantics.md) (execution and re-runs) and [ADR 0030](0030-approval-wait-steps.md) (parking).

## Context

Workflows repeat whole sequences, not just steps: check out a repository and run its tests, deploy and wait for health, post a summary to a channel. [ADR 0033](0033-workflow-library.md) (proposed) shares scripts and single-step presets, but a sequence with its own `needs`, `when`, approvals and retries can only be copied from file to file today. Copies drift, and a fix has to be made in each one.

The deferred-design row named what a sub-workflow needs before it is safe. Recursion must have brakes:

- **run lineage**, which the store already has (`runs.root_run_id`, `parent_run_id` and `depth`, unused so far);
- a **depth cap**;
- a **per-root budget** of descendant runs;
- **cycle rejection**.

The engine already has the other pieces this needs:

- A parked run releases its `workflows.max_runs` slot ([ADR 0030](0030-approval-wait-steps.md)), so a parent can wait for a child without holding a slot the child needs.
- Runs started by hand get their data as `trigger.*`, and the daemon already works out which `trigger.*` fields a workflow requires from the workflow's own references (`icaro run` prompts for the missing ones). A call can pass its inputs the same way.

## Decision

### The step form

**`call`**, a new step form. A step has exactly one of `image`, `uses`, `call`, `approval` or `wait`, plus the forms other proposals add (`await`, `agent`).

```yaml
- id: test
  needs: [checkout]
  call:
    workflow: run-tests
    input:
      repo: ${{ trigger.repo }}
      sha: ${{ steps.checkout.outputs.sha }}
  timeout: 1h
```

- **`workflow`** is a literal workflow name, never an expression. The call graph is therefore static, so validation can check it and the UI can draw it.
- **`input`** becomes the child run's `trigger.*` payload. Values may use `steps` (of `needs` ancestors), `trigger`, `env`, `run` and `workflow`, but not `secrets`: the payload is stored with the child run and shown wherever runs are. A child that needs a secret resolves its own `${{ secrets.* }}`.
- **`timeout`** (optional) is a deadline for the child run, exempt from the `limits.timeout` ceiling like an approval deadline. Past it, the child is cancelled and the step fails. Without one, the step waits for the child however long it takes, approvals in the child included.
- **`retry`** works as on image steps: each attempt starts a new child run.
- `command`, `image`, `env`, `outputs` and `repository` are not allowed. The child has its own steps and workspace, and its outputs are declared by the child (below).

### Being callable

A workflow opts in with a new built-in trigger, **`type: call`**. A workflow without it cannot be called. Validation of the caller checks that:

- the child exists and has a `call` trigger;
- `input` supplies every `trigger.*` field the child references (the same set `icaro run` prompts for). A field the child does not reference is an error, with a nearest-match hint.

A callable workflow can also declare **workflow-level `outputs`**, a map from name to an expression over its steps' outputs, evaluated when the run succeeds:

```yaml
name: run-tests
triggers:
  - type: call
  - type: manual
outputs:
  passed: ${{ steps.test.outputs.passed }}
  report: ${{ steps.test.outputs.report_url }}
```

A `call` step's outputs are the child's workflow outputs, so later steps read `${{ steps.test.outputs.passed }}`. Validation checks those references against the child's declared outputs. Workflow outputs are capped like step outputs (1 MiB in all) and redacted like them.

### Running a call

When a `call` step starts:

1. The engine enqueues the child run with `trigger_kind: call`, `parent_run_id` set to the caller and `root_run_id` taken from the caller. `depth` is the caller's plus one, and the actor is `run:<parent run id>`. The idempotency key is `call:<parent run>:<step>:<attempt>`, so a daemon restart between enqueue and bookkeeping never starts the child twice.
2. The child's workflow version at that moment is recorded on the step run. A **re-run** of the parent ([ADR 0013](0013-workflow-execution-semantics.md)) calls the same child versions the original run recorded, so a re-run repeats what ran.
3. The step waits, and the parent is **parked** like a run on an approval ([ADR 0030](0030-approval-wait-steps.md)). It holds no `workflows.max_runs` slot and no container, so a chain of calls cannot exhaust the slots it needs, even with `max_runs: 1`.
4. When the child reaches a final state, the same transaction that records it sets the parent's `wake_at` to now. The child's outcome maps to the step:

   | Child | Step |
   |---|---|
   | succeeded | succeeded, with the child's workflow outputs as its outputs |
   | failed | failed, with the child's error and a link to it |
   | cancelled | failed with `child run cancelled` |

   A failed step is retried under `retry`, if it has one.

**Concurrency.** The child's `concurrency` policy applies to calls as to any trigger, with one exception: `replace` acts as `queue`. A call must never cancel a run that another parent may be waiting on. With `forbid`, a call while another run of the child is active fails the step with the policy's reason. With `queue`, the step waits until the child's run starts and finishes.

**Cancellation goes down, failure goes up.** Cancelling a run cancels its descendants that are still active, through `parent_run_id`, before it is recorded `cancelled`. Cancelling a child by hand fails the parent's step as above. It does not cancel the parent.

Each run keeps its own run session, workspace and profile ([ADR 0010](0010-run-sessions.md)). A parent and a child share nothing on disk; data crosses only through `input` and workflow outputs.

### Brakes

- **Cycles are rejected at validation.** Saving a workflow fails when its `call` steps, followed through the current versions of the workflows they call, reach the workflow itself. The issue names the path (`a → b → a`).
- **Depth cap.** `workflows.max_call_depth` (default 4) bounds `depth`. A call that would exceed it fails the step. This also catches cycles that a later edit of a child introduces after the parent was validated.
- **Per-root budget.** `workflows.max_runs_per_root` (default 64) bounds the number of runs, retries included, that share a `root_run_id`. A call that would exceed it fails the step. A workflow that fans out by retrying calls cannot multiply without bound.
- **Deleting a workflow** that active runs call cancels those child runs (as delete already does for its own runs), which fails the callers' steps. Validation of a caller whose child is deleted fails with the missing name.

### What is visible

- `get_run`, `icaro runs get` and the run page show a `call` step's child run (link, status, version) and, on a child, its parent and root.
- `icaro runs list --root <id>` lists a whole tree.
- The run graph draws a `call` step as a box that opens the child's graph.
- Logs stay per run. The parent's step log records only the enqueue, the child's id and its outcome.

### Agents

The authoring tools ([architecture §10](../01-architecture.md#10-agent-interface)) understand `call` like any step form. `validate_workflow` reports missing inputs, unknown outputs and cycles, and `get_run` shows the tree.

This ADR adds **no** way for a running step to start a workflow. A child is started only by a `call` step written in a saved workflow version. The `invoke` level of in-container MCP stays deferred. If it is designed later, it reuses this lineage, depth cap and per-root budget.

## Alternatives considered

- **`call_workflow` as a `uses` action.** `uses` actions run in the daemon and return at once ([ADR 0016](0016-pack-loading-action-execution.md)). Waiting on a run, mapping its outcome and parking would all be special cases inside the action executor.
- **Inline expansion** (copy the child's steps into the parent at run time). One run and one graph, but step ids collide, the child's concurrency and approvals lose their meaning, and a child's history disappears into its callers.
- **Expressions in `workflow:`.** Dynamic dispatch is occasionally useful, but it would make the call graph unknowable before a run, so cycle rejection and output checking would move to run time.
- **A declared input list on the child.** The child already states what it needs through its `trigger.*` references, and `icaro run` relies on that. A second declaration would drift from the first.

## Consequences

- A sequence is written once and called from many workflows, with its own approvals, retries, concurrency and history.
- Parked callers keep slots free, so nested calls do not deadlock the engine. A tree of runs still uses one run session and container set per active run.
- New schema: the `call` step form, the `call` trigger type and workflow-level `outputs` (drift-tested against [the contract](../schemas/workflow.schema.json)). New config: `workflows.max_call_depth` and `workflows.max_runs_per_root`. The existing lineage columns get used, and step runs gain the child run id and version.
- A child's edit changes every caller's next run. Validation of the callers catches broken inputs and outputs only when they are validated or run. The child's page lists its callers, and `icaro workflow validate` on a child also validates the workflows that call it.
- Implementation is [Phase 17](../phases/17-call-steps.md).
