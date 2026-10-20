# ADR 0010 — A workflow run is backed by one run session

**Status:** Accepted — resolves [open item O2](../02-decisions-risks.md#open-items)

## Context

The step model says "a step is an image"; the spine says "the unit of work is a session" ([ADR 0002](0002-session-spine.md)). O2 asked how an `image` step maps onto a session and which profile a non-agent image step runs under.

## Decision

- Every workflow run owns exactly one **run session** (`sessions.kind = run`, `sessions.run_id` set). The session is created when the run is claimed and finished when the run ends.
- The run session owns the **workspace**. Steps without `repository` share a scratch directory under `~/.icaro/runs/<run>/workspace`. Steps with `repository` get a worktree resolved once per `(ref, revision)` from the repository cache; steps of the same run with the same pair share it. Both are bind-mounted at `/workspace`.
- Each **image step is one container inside the run session**, created under the container baseline and the run's **profile**: `workflows.profile` from the config (default `workflow`), whose egress allowlist confines the container. An agent step is an image step whose image is the agent base image; it uses the profile home of the agent profile named by `workflows.profile`.
- `uses` steps run in the daemon and touch no container or workspace.
- Startup reconciliation treats run sessions like any other session: a run whose step container still exists is re-attached; otherwise the step is failed and the run continues per its retry policy.

## Consequences

- Workflow runs and interactive agents share one lifecycle, one reconciliation pass and one cleanup path, and appear together in `icaro session list`.
- A workflow cannot pick a profile; the operator does, in config. Widening a workflow's network means editing that profile's allowlist, which keeps the security boundary out of workflow files.
- A per-workflow profile field would need a schema change and a new ADR.
