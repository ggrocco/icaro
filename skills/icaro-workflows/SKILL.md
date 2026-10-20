---
name: icaro-workflows
description: Author, validate, run and debug Ícaro workflows through the icaro MCP server. Use when asked to automate something with Ícaro, create or change a workflow, react to GitHub/GitLab/Slack/Jira/Linear/Sentry/Datadog events, or figure out why a workflow run failed.
---

# Authoring Ícaro workflows

Ícaro workflows are YAML DAGs of steps. Each step is either an `image`
step (a container) or a `uses` step (an integration action the daemon
runs over HTTP). You work through the `icaro` MCP server's tools; the
schema and the validator are the source of truth, not your memory.

## The loop

1. **Fetch the contract.** Call `get_workflow_schema`, then
   `list_integrations`. For every action you plan to use, call
   `get_integration` and read its typed inputs and outputs. Never write
   fields from memory: unknown fields are rejected.
2. **Check secrets.** Call `list_secrets` (names and host bindings only).
   If a step needs a secret or an integration credential that is missing,
   stop and ask the human to create it (`icaro secret set NAME` or
   `icaro credential add <integration>`). Never ask for the value and
   never put a secret value in a workflow.
3. **Draft, then validate until clean.** Call `validate_workflow` with
   your YAML. Each issue has a JSON Pointer `path`, a `message` and often
   a `hint` ("did you mean …?"). Fix exactly what the path points at and
   validate again. Do not save a workflow with issues.
4. **Create and run small.** Call `create_workflow` (or `update_workflow`
   for an existing one), then `run_workflow` with a small `trigger`
   payload. Manual runs must supply every `trigger.*` field the workflow
   references.
5. **Debug with `get_run`.** It returns status, per-step outputs and a
   size-capped log tail. Fix the workflow and repeat from step 3.

You cannot delete workflows or runs; that is for humans.

## Shape of a workflow

```yaml
version: 1
name: review-pr                 # ^[a-z][a-z0-9-]{2,62}$
description: Comment on new PRs
concurrency: queue              # forbid | queue | replace | allow
env:
  STAGE: ci
triggers:
  - integration: github
    event: pull_request.opened
    filter: trigger.base_branch == 'main'
  - type: manual
steps:
  - id: checks
    image: mirror.gcr.io/library/alpine:3.20
    repository: { ref: "${{ trigger.repo_url }}", revision: "${{ trigger.head_sha }}" }
    command: ./scripts/check.sh > /icaro/out.txt; printf '{"ok":true}' > /icaro/output.json
    outputs:
      ok: json:ok
    timeout: 10m
    retry: 1
  - id: comment
    needs: [checks]
    when: steps.checks.outputs.ok == true
    uses: github.create-issue-comment
    input:
      repo: "${{ trigger.repo }}"
      number: "${{ trigger.pr_number }}"
      body: "Checks passed for ${{ trigger.head_sha }}"
```

## Rules the validator enforces

- A step has exactly one of `image` or `uses`. `command`, `env` and
  `outputs` are for image steps only; a `uses` step's outputs come from
  the action.
- `${{ steps.<id>.outputs.<key> }}` may reference only steps listed
  (transitively) in `needs`, and only declared outputs.
- `when` and `filter` are boolean expressions: `== != < <= > >= && || !`,
  literals, references, and the functions `contains startsWith endsWith
  lower upper trim length toJSON fromJSON join split format coalesce
  string number has`. A missing key is an error; guard optional fields
  with `has(trigger.x)`.
- `${{ secrets.NAME }}` is resolved by the daemon and redacted from logs.
- Image steps read `/icaro/input.json` (the step's `input`) and
  `ICARO_INPUT_<NAME>`, write `/icaro/output.json` (a JSON object, at most
  1 MiB), share `/workspace` with the other steps of the run, and succeed
  with exit code 0. Output captures: `json:<dot.path>`, `stdout`,
  `stdout:last-line`, `file:<path under /workspace>`.
- Image steps run under the operator's workflow profile, whose egress
  allowlist decides what the container can reach. If a step needs a host
  that is not allowed, ask the human to add it to the profile; you cannot
  widen it from the workflow.
