# Workflows

A workflow is a YAML DAG of steps that orchestrates sessions. It is not a general-purpose CI system. Each run gets one **run session** whose workspace every step shares ([ADR 0010](../blueprint/docs/adr/0010-run-sessions.md)). The contract is the published JSON Schema: `icaro schema` prints it, and so do `GET /api/v1/schema/workflow` and the MCP tool `get_workflow_schema`. Unknown fields are rejected.

```sh
icaro workflow validate file.yaml   # structured issues, nothing stored
icaro workflow apply file.yaml      # validate and store as a new version
icaro run NAME --wait               # start a manual run and stream its logs
```

## A first workflow

Code blocks marked `yaml workflow` in these docs are validated by the test suite, and their images must be pinned by digest.

```yaml workflow
version: 1
name: hello
description: Greet whoever the trigger names
triggers:
  - type: manual
steps:
  - id: greet
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    input:
      who: ${{ trigger.who }}
    command: |
      echo "hello $ICARO_INPUT_WHO"
      printf '{"greeting":"hello %s"}' "$ICARO_INPUT_WHO" > /icaro/output.json
    outputs:
      greeting: json:greeting
  - id: shout
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    needs: [greet]
    env:
      GREETING: ${{ steps.greet.outputs.greeting }}
    command: echo "$GREETING" | tr a-z A-Z
```

## Top-level fields

| Field | Meaning |
|---|---|
| `version` | Always `1` |
| `name` | `^[a-z][a-z0-9-]{2,62}$`; applying the same name stores a new version |
| `description` | Free text, at most 500 characters |
| `concurrency` | `allow` (default), `forbid`, `queue` or `replace`; see [Concurrency](#concurrency) |
| `env` | Environment for every step; values may use `${{ }}`, including `${{ secrets.NAME }}` |
| `triggers` | `type: manual`, `type: cron` with `cron:`, or an integration event; see [Integrations](integrations.md) |
| `steps` | The DAG; at least one step |

## Steps

A step has an `id` (`^[a-z][a-z0-9-]{1,62}$`) and exactly one of five forms.

- An **`image` step** runs a container. `command` runs as `/bin/sh -c <command>`; without it, the image's entrypoint runs.
- An **`agent` step** runs an agent CLI (Claude Code or Codex) headless, under a profile the operator allows; see [Agent steps](#agent-steps).
- A **`uses` step** calls an integration action, such as `uses: slack.post-message`, inside the daemon over HTTP. No container runs. `input` is checked against the action's typed inputs, and its outputs come from the manifest. Use `icaro integration show <name>` to list them.
- An **`approval` step** waits until a person approves or rejects it; see [Approvals and waits](#approvals-and-waits).
- A **`wait` step** waits for a duration, such as `wait: 2h`.

Every form takes the same scheduling fields, except as noted:

| Field | Meaning |
|---|---|
| `needs` | Step ids that must finish first. Independent steps run in parallel, within the [parallelism limits](#parallelism). |
| `when` | A boolean expression; false records the step as `skipped`. It is evaluated once, after every need is final. |
| `retry` | 0–5 extra attempts. The backoff before attempt k+1 is 2s·2^(k−1), capped at 1 minute. Not on approval or wait steps. |
| `timeout` | Per attempt, for example `90s`, `10m` or `1h`. The default and ceiling is `limits.timeout` (30m). On an approval step it is the decision deadline instead, with no ceiling; a wait step has none. |
| `repository` | `{ref, revision}`. The repository is fetched once per run into the repository cache, checked out as a worktree, and mounted at `/workspace` for this step and any other step of the run with the same ref and revision. Both fields may use `${{ }}` (for example `https://github.com/${{ trigger.repo }}.git`) with the same root and `needs` rules as other fields, but not `secrets`. A rendered value that is empty (ref), starts with `-`, contains whitespace or carries a secret fails the step before git runs. |
| `env` | Image and agent steps. Merged over the workflow `env`. |
| `input` | Inputs. Image steps receive them as `/icaro/input.json` and `ICARO_INPUT_<NAME>`; for `uses` steps they are the action's typed inputs. |
| `outputs` | Image and agent steps. A map from name to capture source; see below. |

A step runs only when each of its `needs` **succeeded** or was **skipped by its own `when`**. If a need failed, was cancelled, or did not run, the step is recorded as `skipped` with `not run: needs <id> <status>`, its dependents skip in turn, and the run fails. If the run fails before any step starts (its session, egress or workflow `env` could not be set up), every step is recorded as `skipped` with `not run: <run error>`, or as `cancelled` if the run was cancelled. The details are in [ADR 0013](../blueprint/docs/adr/0013-workflow-execution-semantics.md).

## The image-step contract

Any image works as a step; it needs to know nothing about Ícaro.

| Channel | Mechanism |
|---|---|
| Inputs | `ICARO_INPUT_<NAME>` environment variables plus `/icaro/input.json` (the step's resolved `input`) |
| Outputs | Write a JSON object (at most 1 MiB) to `/icaro/output.json`, then capture values with `outputs` |
| Files | `/workspace` is shared by every step of the run |
| Status | Exit code `0` means success |
| Logs | stdout and stderr, captured with secret values redacted |

Each container runs under the hardening baseline: non-root, no capabilities, `no-new-privileges`, a seccomp profile, a read-only root filesystem with a tmpfs `/tmp`, and pids/memory/CPU/open-file limits. Its network is the egress allowlist of the profile named by `workflows.profile` (default `workflow`, which allows nothing). A workflow cannot widen any of this; the operator changes it in the config.

### Output capture

| Form | Value |
|---|---|
| `json:<path>` | A value inside `/icaro/output.json`: `json:pr.url`, `json:items.0.id` or `json:items[0].id`. `json:$` is the whole document. The JSON type is kept. |
| `stdout` | The whole stdout, trimmed, capped at 1 MiB |
| `stdout:last-line` | The last non-empty stdout line |
| `file:<path>` | A file under `/workspace`, trimmed, capped at 1 MiB. Escaping the workspace, including through a symlink, fails the step. |

Later steps read the values as `${{ steps.<id>.outputs.<name> }}`, but only from their `needs` ancestors and only outputs the step declares. Validation enforces both.

## Approvals and waits

An approval step stops the run until a person decides; a wait step stops it for a time. Neither runs a container.

```yaml workflow
version: 1
name: release
triggers:
  - type: manual
steps:
  - id: build
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    command: echo '{"tag":"v1.2.0"}' > /icaro/output.json
    outputs: {tag: "json:tag"}
  - id: review
    needs: [build]
    approval:
      message: "Release ${{ steps.build.outputs.tag }}?"
    timeout: 24h
  - id: soak
    needs: [review]
    wait: 30m
  - id: publish
    needs: [soak]
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    env:
      APPROVER: ${{ steps.review.outputs.approved_by }}
    command: echo "publishing, approved by $APPROVER"
```

- **Deciding.** `icaro runs approve RUN STEP [--comment TEXT]` approves; `icaro runs reject RUN STEP --comment WHY` rejects and needs a reason. The run page in the web UI has the same buttons, and `icaro runs logs RUN -f` prints the request when the run reaches it. Deciding needs a `write` token. The approver is recorded as the caller (`token:<name>`, or `token:local` on the daemon's socket).
- **Outcomes.** An approval outputs `approved_by` and `comment` to the steps after it. A rejection fails the step with `rejected by <who>: <comment>`, and the steps that need it are skipped. An approval left undecided past its `timeout` fails with `approval expired`; without a `timeout` it waits until it is decided or the run is cancelled. A decision is final.
- **Message.** `approval.message` (at most 2000 characters) may use `trigger.*`, `env`, `run`, `workflow` and the outputs of the step's `needs` ancestors, but not `secrets`: the message is shown to whoever can read runs. The default is `Approve step <id>?`.
- **Waits.** `wait` takes `s`, `m` or `h` units, at most `720h` (30 days). It ends on time even across a daemon restart: the step's start time is stored.
- **Parked runs.** While a run has only approval and wait steps left, its status is `waiting` and it holds no `workflows.max_runs` slot, so other runs go ahead. It still counts as active for its workflow's [concurrency](#concurrency) policy. A decision, the end of a wait, or `icaro runs cancel` resumes it. Steps that do not depend on a pending approval keep running while it waits.

## Agent steps

An agent step gives a task to an agent CLI and lets it work in `/workspace`, under the operator's login and network policy ([ADR 0037](../blueprint/docs/adr/0037-agent-steps.md)). Its final answer is the built-in output `result`.

```yaml workflow
version: 1
name: pr-review
triggers:
  - type: manual
steps:
  - id: review
    repository:
      ref: ${{ trigger.repo }}
      revision: ${{ trigger.sha }}
    agent:
      profile: reviewer
      prompt: |
        Review the change in /workspace against main. Write the risks you
        find to /icaro/output.json as {"risks": "..."}.
      params:
        model: sonnet
        max_turns: 30
    outputs:
      risks: "json:risks"
    timeout: 20m
  - id: report
    needs: [review]
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    env:
      SUMMARY: ${{ steps.review.outputs.result }}
      RISKS: ${{ steps.review.outputs.risks }}
    command: echo "$SUMMARY" && echo "$RISKS"
```

- **Enabling a profile.** Agent steps are off until an operator lists the profiles workflows may use, in the config: `workflows: {agent_profiles: [reviewer]}`. A step without `profile` uses the first one. A workflow cannot pick any other profile: a profile decides whose login, egress allowlist and credentials the agent gets. `icaro config show` lists them; `icaro doctor` checks that each is signed in (or injects an API key), reaches its provider and has a recent enough CLI.
- **The harness is the profile's agent.** `params` are the CLI's declared knobs, checked by the validator: `model` and `max_turns` for Claude Code, `model` for Codex. There is no free-form flag list; a different invocation is an image step.
- **The prompt** may use `steps` (of `needs` ancestors), `trigger`, `env`, `run` and `workflow`, never `secrets`: it is stored with the run, shown in the UI and sent to the provider. Pass a secret the agent needs through `env`. The rendered prompt is fed to the CLI on stdin.
- **Outputs.** `result` is the agent's last message (at most 1 MiB). Other outputs are captured from `/icaro/output.json` or the workspace as on image steps, so tell the agent to write them.
- **Each attempt starts clean.** The step gets a fresh home layer over the profile's signed-in home, discarded when the attempt ends, so nothing it writes there reaches another step or run. `/workspace` keeps what a failed attempt wrote, as on image steps.
- **Network and credentials.** The step uses its profile's egress allowlist, not `workflows.profile`'s. With an `inject` profile the API key stays on the host: the CLI gets a placeholder token and the daemon's relay.
- **The image** is the profile's, resolved to a digest when the run starts and used by every attempt and agent step of that profile in the run, even if the tag moves. The run shows the digest.
- **Memory.** The step gets the memory MCP tools unless it sets `memory: false`. The daemon scopes them to the run's project, else the step's repository, else the workflow.
- **Failures.** An expired login fails the step with **Sign in required** and the `icaro login <profile>` command, without a retry. A usage limit or a CLI error fails the attempt, and `retry` runs it again from a fresh home. While a step of a profile runs, `icaro login <profile>` is refused.
- **Usage.** The tokens the CLI reports are recorded on the step, shown on the run page and counted by `icaro usage` (per run and step). Subscription profiles show tokens only; `inject` profiles also show the CLI's cost, labelled an estimate.
- **Untrusted input.** A prompt that reads `trigger.*` in a workflow with an integration trigger carries text an outsider may have written, so prompt injection is the expected case. Validation warns, naming the profile. The agent can still reach only its profile's allowlist, its workspace, its own home layer and its memory scope; it has no daemon API and no secrets you did not pass in `env`. Keep the profile's allowlist narrow and pass the agent only what it needs.

## Expressions

`${{ … }}` is one small language shared with integration manifests ([ADR 0009](../blueprint/docs/adr/0009-expression-language.md)). Expressions are parsed when you validate, and a missing key is an error, never an empty string.

- **Roots:** `steps`, `trigger`, `env`, `secrets`, `run` (`run.id`), `workflow` (`workflow.name`, `workflow.version`). A `when` cannot read `secrets`: whether a step ran is visible to anyone who can see the run, so a condition on a secret would leak it a bit at a time.
- **Operators:** `== != < <= > >= && || !`, parentheses, literals (`'text'`, `"text"`, numbers, `true`, `false`, `null`) and indexing (`headers['x-github-event']`, `trigger.labels[0]`). There is no arithmetic.
- **Functions:** `contains startsWith endsWith lower upper trim length toJSON fromJSON join split format coalesce string number`, plus `has(ref)`, the only construct for which a missing key is not an error.
- **Templates:** a string that is exactly one `${{ }}` keeps the value's type, so `number: ${{ trigger.pr_number }}` passes a number. Mixed text renders values as strings, and objects as JSON.
- **Conditions** (`when`, trigger `filter`) must evaluate to a boolean. There is no truthiness.

```yaml workflow
version: 1
name: gated
triggers:
  - type: manual
steps:
  - id: check
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    command: |
      printf '{"changed": true}' > /icaro/output.json
    outputs:
      changed: json:changed
  - id: deploy
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    needs: [check]
    when: steps.check.outputs.changed == true && (!has(trigger.dry_run) || trigger.dry_run == false)
    command: echo deploying
```

### Keep untrusted text out of the shell

`trigger.*` values and step outputs can carry text that someone outside your team controls, such as a PR title or an issue body, and `command` is a shell script. **Never interpolate them into `command`.** Pass them through `env` and quote the variable:

```yaml
    env:
      TITLE: ${{ trigger.title }}
    command: |
      echo "reviewing: $TITLE"
```

Write multi-line commands, and any command that contains ` #`, as a block scalar (`|`). In a plain YAML scalar, ` #` starts a comment and silently truncates the command.

## Secrets

`${{ secrets.NAME }}` in `env` is resolved by the daemon at run time. Its value is never stored in the workflow, and it is redacted from logs and captured outputs. Create secrets with `icaro secret set NAME` (the value is read from stdin or a hidden prompt). A workflow that references a secret that does not exist fails validation, with a hint to create it. Integration credentials for `uses` steps are separate and host-bound: a secret with a host binding, or one named `<integration>.<name>` for an installed integration, cannot be interpolated, and the run fails if one is referenced anyway. Call the integration through its `uses:` actions; see [Integrations](integrations.md#credentials).

## Runs

- **Manual runs.** `icaro run NAME` takes the `trigger.*` payload from `--set key=value` (numbers, booleans and JSON are parsed), `--trigger-json` or `--trigger-file`. Fields the workflow reads outside `has()` are required, unless the read sits behind a `has()` check of the same field (`has(trigger.label) && trigger.label == 'bug'`, `!has(trigger.x) || trigger.x == 1`): the CLI prompts for missing ones (unless `--no-prompt`), and the API answers 400 with the list. `icaro workflow apply` prints them as `manual runs need: --set who=...`.
- **Watching.** `--wait` streams the logs and exits non-zero unless the run succeeds; with `--json` it prints only the final run, and the exit status is the same. `icaro workflow validate --json` likewise prints the issues and exits non-zero for an invalid file. `icaro runs get ID`, `icaro runs logs ID [STEP] [-f]` and `icaro runs cancel ID` inspect and stop runs, and `icaro runs approve` / `reject` decide [approval steps](#approvals-and-waits). Cancelling kills the step containers. `icaro runs rerun ID` runs a run again on the workflow version it used, with the same trigger payload, even after the workflow has changed.
- **Streaming API.** `GET /api/v1/runs/{id}/events?logs=1` is a Server-Sent Events stream with one stream per run. It sends a `snapshot` (with the log backlog), then `step` and `log` events, and finally `end`. The web UI uses it, so it never polls.
- **Restarts.** A step's container id is recorded before the container starts. After a daemon restart, a running step re-attaches to its container. If the container is gone, the attempt fails and `retry` decides. `uses` steps are therefore at-least-once across restarts.
- **Versions.** Every apply stores a version (`icaro workflow versions NAME [N]`), and a run always executes the version it was started with.
- **Deleting.** `icaro workflow delete NAME` cancels the workflow's queued, running and waiting runs. Its runs are kept, and so are the versions they executed, so their history stays readable.

## Concurrency

| Policy | While a run of the workflow is active… |
|---|---|
| `allow` (default) | Start another run |
| `forbid` | Skip the new run; webhook deliveries record the reason |
| `queue` | Wait until the active run finishes |
| `replace` | Cancel the active runs; the new run starts once they have stopped |

The policy is checked and the run created in one transaction, so concurrent triggers cannot both slip past `forbid`. Webhook and cron runs carry an idempotency key, so a retried delivery or a repeated cron tick never starts a second run.

## Parallelism

Three config keys bound how much runs at once. A workflow cannot change them.

| Key | Default | Caps |
|---|---|---|
| `workflows.concurrency` | `4` | Step containers and actions running at once, across all runs |
| `workflows.max_runs` | `0` (= `workflows.concurrency`) | Runs executing at once; further runs wait in the queue |
| `workflows.run_parallelism` | `0` (no extra cap) | Parallel steps within one run, so one wide DAG cannot take every slot |

```yaml
workflows:
  concurrency: 8
  max_runs: 4
  run_parallelism: 2
```

## Scheduling

```yaml workflow
version: 1
name: nightly
concurrency: forbid
triggers:
  - type: cron
    cron: "15 3 * * *"
  - type: manual
steps:
  - id: sweep
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    command: echo "sweeping"
```

Cron expressions have five fields (minute, hour, day of month, month, day of week) or use an `@daily`-style descriptor. A cron run's `trigger` holds only `scheduled_at`, `fired_at` (RFC 3339) and `late` (fired more than a minute after its time), so a workflow with a cron trigger can read other `trigger.*` fields only behind `has()`; validation rejects it otherwise. The scheduler persists each trigger's next fire time. A fire missed while the daemon was down runs **once** on restart, however many ticks were missed. `icaro schedules` lists the triggers with their next fire times.

## More examples

[`examples/`](../examples/) has a parallel DAG with outputs and `when` (`dag.yaml`), the greeting above (`hello.yaml`), and a cron workflow (`nightly.yaml`).
