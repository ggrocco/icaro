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

A step has an `id` (`^[a-z][a-z0-9-]{1,62}$`) and exactly one of two forms.

- An **`image` step** runs a container. `command` runs as `/bin/sh -c <command>`; without it, the image's entrypoint runs. An agent step is an image step whose image is the agent base image and whose command runs the agent CLI headless.
- A **`uses` step** calls an integration action, such as `uses: slack.post-message`, inside the daemon over HTTP. No container runs. `input` is checked against the action's typed inputs, and its outputs come from the manifest. Use `icaro integration show <name>` to list them.

Both forms take the same scheduling fields:

| Field | Meaning |
|---|---|
| `needs` | Step ids that must finish first. Independent steps run in parallel, up to `workflows.concurrency` per daemon. |
| `when` | A boolean expression; false records the step as `skipped`. It is evaluated once, after every need is final. |
| `retry` | 0–5 extra attempts. The backoff before attempt k+1 is 2s·2^(k−1), capped at 1 minute. |
| `timeout` | Per attempt, for example `90s`, `10m` or `1h`. The default and ceiling is `limits.timeout` (30m). |
| `repository` | `{ref, revision}`. The repository is fetched once per run into the repository cache, checked out as a worktree, and mounted at `/workspace` for this step and any other step of the run with the same ref and revision. |
| `env` | Image steps only. Merged over the workflow `env`. |
| `input` | Inputs. Image steps receive them as `/icaro/input.json` and `ICARO_INPUT_<NAME>`; for `uses` steps they are the action's typed inputs. |
| `outputs` | Image steps only. A map from name to capture source; see below. |

A step runs only when each of its `needs` **succeeded** or was **skipped by its own `when`**. If a need failed, was cancelled, or did not run, the step is recorded as `skipped` with `not run: needs <id> <status>`, its dependents skip in turn, and the run fails. The details are in [ADR 0013](../blueprint/docs/adr/0013-workflow-execution-semantics.md).

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
- **Watching.** `--wait` streams the logs and exits non-zero unless the run succeeds; with `--json` it prints only the final run, and the exit status is the same. `icaro workflow validate --json` likewise prints the issues and exits non-zero for an invalid file. `icaro runs get ID`, `icaro runs logs ID [STEP] [-f]` and `icaro runs cancel ID` inspect and stop runs. Cancelling kills the step containers.
- **Streaming API.** `GET /api/v1/runs/{id}/events?logs=1` is a Server-Sent Events stream with one stream per run. It sends a `snapshot` (with the log backlog), then `step` and `log` events, and finally `end`. The web UI uses it, so it never polls.
- **Restarts.** A step's container id is recorded before the container starts. After a daemon restart, a running step re-attaches to its container. If the container is gone, the attempt fails and `retry` decides. `uses` steps are therefore at-least-once across restarts.
- **Versions.** Every apply stores a version (`icaro workflow versions NAME [N]`), and a run always executes the version it was started with.

## Concurrency

| Policy | While a run of the workflow is active… |
|---|---|
| `allow` (default) | Start another run |
| `forbid` | Skip the new run; webhook deliveries record the reason |
| `queue` | Wait until the active run finishes |
| `replace` | Cancel the active runs, then start |

Webhook and cron runs carry an idempotency key, so a retried delivery or a repeated cron tick never starts a second run.

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

Cron expressions have five fields (minute, hour, day of month, month, day of week) or use an `@daily`-style descriptor. The scheduler persists each trigger's next fire time. A fire missed while the daemon was down runs **once** on restart, however many ticks were missed. `icaro schedules` lists the triggers with their next fire times.

## More examples

[`examples/`](../examples/) has a parallel DAG with outputs and `when` (`dag.yaml`), the greeting above (`hello.yaml`), and a cron workflow (`nightly.yaml`).
