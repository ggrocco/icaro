# ADR 0037 — Agent steps: an agent CLI as a first-class workflow step

**Status:** Proposed. Would lift [MVP cut 8](../02-decisions-risks.md#mandatory-mvp-cuts) for one more step form, `agent`, and take the "`agent` step type" row out of the [deferred designs](../02-decisions-risks.md#deferred-designs). Amends [ADR 0010](0010-run-sessions.md) on which profile a step runs under. Builds on the headless agent adapter of [ADR 0035](0035-workflow-assistant-chat.md), and is gated by the same spike, S11.

## Context

Cut 8 says an agent in a workflow is an image step "whose image is the agent base image and whose command runs the agent CLI headless". [ADR 0010](0010-run-sessions.md) and [architecture §5](../01-architecture.md#5-agent-container) record what that means today: an image step runs with `HOME=/home` and no profile home, no memory MCP socket, and the egress allowlist of `workflows.profile`, whose default (`workflow`) has no agent and no egress at all.

So the only way to run an agent in a workflow today is to point `workflows.profile` at an agent profile, which widens the network of every step of every workflow, and to pass the provider key as `${{ secrets.* }}` in `env`. That puts the key inside the container, which is the thing [ADR 0028](0028-credential-injection.md) exists to avoid. The login done once with `icaro login` is not reused. The workflow author also has to know each CLI's headless flags, output format and exit codes, and nothing validates them.

[ADR 0035](0035-workflow-assistant-chat.md) (proposed) already gives each agent in the `internal/creds` catalog a **headless adapter**: the flags for a one-shot, non-interactive turn, pinned per CLI version by spike S11, the prompt on stdin, and a parser that normalizes the CLI's event stream (`text`, `tool_call`, `usage`, `error`, `done`, …). An agent step needs the same adapter, plus what is specific to a workflow step.

The deferred-design row named the pieces: a **harness catalog** with **declared params**, so each CLI's knobs are validatable, and **images pinned by digest**.

## Decision

### The step form

**`agent`**, a new step form. A step has exactly one of `image`, `uses`, `agent`, `approval` or `wait`, plus `await` if [ADR 0032](0032-await-event-steps.md) is accepted.

```yaml
- id: review
  needs: [diff]
  repository:
    ref: https://github.com/${{ trigger.repo }}.git
    revision: ${{ trigger.head_sha }}
  agent:
    profile: reviewer
    prompt: |
      Review the change in /workspace against main.
      Summarize the risks in /icaro/output.json as {"summary": "..."}.
    params:
      model: sonnet
      max_turns: 30
  outputs:
    summary: "json:summary"
  timeout: 20m
```

- **`profile`** names the agent profile the step runs under: its agent CLI, image, profile home, egress allowlist, `inject` keys and runtime. It must be listed in a new operator setting, **`workflows.agent_profiles`**, which is empty by default. When the step omits `profile`, the first entry is used. A workflow therefore never chooses its own security boundary: the operator lists, in config, which profiles workflows may run agents under. This amends ADR 0010's "a workflow cannot pick a profile" to "a workflow can pick only among the profiles the operator listed for agent steps". Image and `uses` steps keep `workflows.profile`.
- **The harness is the profile's `agent`** (`claude`, `codex`). The step does not name it, so the profile and its credentials cannot disagree. A profile with no agent cannot be listed in `workflows.agent_profiles`.
- **`prompt`** is a template over `steps` (of `needs` ancestors), `trigger`, `env`, `run` and `workflow`, but not `secrets`. The prompt is stored with the step run, shown in the UI and sent to the provider, so it is not a place for a secret. The rendered prompt is written to `/icaro/prompt.md` in the step's I/O volume, so it is kept with the attempt, and fed to the CLI on stdin, as ADR 0035's adapter does: never on the command line, where it would be size-limited and visible to `ps`.
- **`params`** are the harness's declared knobs, validated against the catalog. An unknown param or a wrong type is a validation error with a nearest-match hint. There is no free-form `args`: an invocation the catalog does not cover is an image step (or, with [ADR 0033](0033-workflow-library.md), a preset).
- **`memory`** (optional, default `true`): whether the step gets the memory MCP server, as interactive sessions do ([ADR 0019](0019-memory-mcp-transport.md)). The scope is derived server-side from the run session's identity and the step's repository, so a step cannot widen it.
- `outputs`, `env`, `input`, `timeout`, `retry`, `repository`, `needs` and `when` work as on image steps. `command` and `image` are not allowed: the harness supplies the command and the profile the image.

### The harness catalog

The catalog is the agent table that already drives sessions (`internal/creds`, keyed by a profile's `agent`), with ADR 0035's headless adapter. It ships in the binary and is versioned with the engine, because each entry is tested against the real CLI (spikes S4 and S11) and against credential injection. An agent step uses the adapter's **one-shot** invocation (no resume; see the alternatives) and its stream parser, and the catalog adds per harness:

- **Unattended mode:** the permission mode fixed for a run with no person present, inside the container's confinement.
- **Params:** name, type (`string`, `int`, `bool` or an enum), and the flag each maps to. The first set is small (`model` and `max_turns` for both CLIs) and grows one validated param at a time.
- **Result capture:** the final answer in the normalized stream becomes the built-in output **`result`** (capped at 1 MiB, like `/icaro/output.json`), beside any `outputs` the step declares from `/icaro/output.json`.
- **Exit mapping:** which exit codes and stream events (`error`, `auth_required`, `rate_limited`) fail the attempt. A failed attempt is retried under `retry` like an image step; `auth_required` is not retried, and names `icaro login <profile>` as the fix.
- **Usage:** the stream's `usage` events are recorded on the step run, so a run's token use is attributed to it ([ADR 0022](0022-usage-from-local-logs.md)) without parsing the discarded home.

The normalized stream, which carries model output, is untrusted, as in ADR 0035: it is parsed, size-capped and redacted into the step log, never evaluated.

A profile can still set a custom `image`. The catalog says what must be in it (the CLI on `PATH`), and `icaro doctor` checks it.

### Running it

An agent step is one container in the run session ([ADR 0010](0010-run-sessions.md)), like an image step, with three differences:

1. **Home.** The step gets a fresh writable layer over its profile's golden seed ([ADR 0005](0005-persistent-home-egress.md), [ADR 0012](0012-egress-proxy-and-home-layers.md)), mounted as the CLI's home. The layer is discarded when the attempt ends. So the login is reused, and nothing a step writes (including a refreshed token) persists or reaches another step. A running agent step counts as a running overlay session of its profile, so `icaro login` refuses to rewrite the seed under it, as it does for interactive sessions.
2. **Network.** The step's container is attached to its **agent profile's** egress proxy, not the run's `workflows.profile` proxy. With `inject`, the API key stays on the host and the container gets the placeholder and base URL ([ADR 0028](0028-credential-injection.md)). A run whose agent steps use two profiles has two attachments.
3. **Image.** The profile's image is resolved to the digest pinned in the `images` table when the run starts, and that digest is recorded on the step run. Every attempt of the step, and every agent step of the run under the same profile, runs that exact image, even if the tag moves mid-run.

`/workspace`, `/icaro/input.json`, `/icaro/output.json`, the container baseline, `limits` and log redaction are those of an image step. The agent's transcript goes to the step's log like any output.

### Untrusted input

A prompt that interpolates `trigger.*` from an integration event carries text an outsider wrote, so prompt injection is the expected case, not an edge case. The design bounds what an injected agent can do; it does not try to detect injection:

- The agent can reach only its profile's egress allowlist. With `inject`, it can use the provider key through the proxy but cannot read it.
- It has the step's workspace, its own discarded home layer, and its memory scope.
- It has no daemon API, no secrets except those the step passes explicitly in `env`, and no in-container workflow tools. ADR 0035 proposes authoring tools for assistant sessions only, where a person applies every write; for agent steps, in-container MCP stays [deferred](../02-decisions-risks.md#deferred-designs).
- Validation **warns** (it does not fail) when an agent step's prompt references `trigger.*` and the workflow has an integration trigger. The warning names the profile whose egress and credentials the agent will have.

## Alternatives considered

- **Keep agents as image steps and fix the home mount.** That would close the gap with ADR 0010, but every workflow would still carry CLI-specific flags, output parsing and exit-code knowledge that nothing validates, and a profile switch would still apply to every step of a run.
- **A `harness:` field on the step, separate from the profile.** It would let a workflow pair any CLI with any profile's credentials and egress, which is a security decision ADR 0010 deliberately kept in config.
- **Harnesses as data in packs**, like integrations. Attractive for third-party CLIs, but each harness touches credential handling and S4's per-CLI findings, so the first catalog is built in. Custom invocations fit ADR 0033's presets instead.
- **A persistent home per run**, so later agent steps can resume an earlier step's conversation. Useful, but it turns step isolation into shared mutable state across steps. Left out of this ADR.

## Consequences

- A workflow can run Claude Code or Codex with the operator's login and network policy, typed params and a captured result, and with the provider key never inside the container when the profile injects it.
- Agent steps are off until the operator lists a profile in `workflows.agent_profiles`. Enabling one is an explicit decision about which credentials and egress automated, possibly untrusted, prompts get.
- The engine needs a per-step egress attachment and per-step home layers, where today a run has one attachment and steps have no home. When this lands, ADR 0010 and architecture §5 are updated to describe agent steps.
- Agent steps and assistant chats share one adapter per CLI. A CLI upgrade that changes flags or the event format is fixed in one place and caught by the same recorded-stream fixtures.
- `agent` is a new step kind for the schema (drift-tested against [the contract](../schemas/workflow.schema.json)), the validator, the API, the CLI and the web UI's run views. `result` is a new built-in output.
- Agent runs cost money and are nondeterministic. A retry reruns the agent from scratch with a fresh home layer, but `/workspace` keeps what the failed attempt wrote, as it does for image steps.
- Implementation is [Phase 16](../phases/16-agent-steps.md), which reuses Phase 14's agent adapters.
