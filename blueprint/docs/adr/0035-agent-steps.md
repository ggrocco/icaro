# ADR 0035 — Agent steps: an agent CLI as a first-class workflow step

**Status:** Proposed. Would lift [MVP cut 8](../02-decisions-risks.md#mandatory-mvp-cuts) for one more step form, `agent`, and take the "`agent` step type" row out of the [deferred designs](../02-decisions-risks.md#deferred-designs). Amends [ADR 0010](0010-run-sessions.md) on which profile a step runs under.

## Context

Cut 8 says an agent in a workflow is an image step "whose image is the agent base image and whose command runs the agent CLI headless". [ADR 0010](0010-run-sessions.md) and [architecture §5](../01-architecture.md#5-agent-container) add that such a step uses the profile home of the agent profile, so the login done once with `icaro login` is reused by workflow steps.

The engine does not do that. An image step runs with:

- `HOME=/home` and no profile home mounted (`internal/workflow/engine/image.go`);
- no memory MCP socket;
- the egress allowlist of `workflows.profile`, whose default (`workflow`) has no agent and no egress at all.

So the only way to run an agent in a workflow today is to point `workflows.profile` at an agent profile, which widens the network of every step of every workflow, and to pass the provider key as `${{ secrets.* }}` in `env`. That puts the key inside the container, which is the thing [ADR 0028](0028-credential-injection.md) exists to avoid. The workflow author also has to know each CLI's headless flags, output format and exit codes, and nothing validates them.

The deferred-design row named the pieces: a **harness catalog** with **declared params**, so each CLI's knobs are validatable, and **images pinned by digest**.

## Decision

### The step form

**`agent`**, a new step form. A step has exactly one of `image`, `uses`, `agent`, `approval`, `wait` (or `await`, if [ADR 0032](0032-await-event-steps.md) is accepted).

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
- **`prompt`** is a template over `steps` (of `needs` ancestors), `trigger`, `env`, `run` and `workflow`, but not `secrets`. The prompt is stored with the step run, shown in the UI and sent to the provider, so it is not a place for a secret. The rendered prompt is written to `/icaro/prompt.md` in the step's I/O volume and given to the CLI from there, never on the command line (where it would be size-limited and visible to `ps`).
- **`params`** are the harness's declared knobs, validated against the catalog. An unknown param or a wrong type is a validation error with a nearest-match hint. There is no free-form `args`: an invocation the catalog does not cover is an image step (or, with [ADR 0033](0033-workflow-library.md), a preset).
- **`memory`** (optional, default `true`): whether the step gets the memory MCP server, as interactive sessions do ([ADR 0019](0019-memory-mcp-transport.md)). The scope is derived server-side from the run session's identity and the step's repository, so a step cannot widen it.
- `outputs`, `env`, `input`, `timeout`, `retry`, `repository`, `needs` and `when` work as on image steps. `command` and `image` are not allowed: the harness supplies the command and the profile the image.

### The harness catalog

The catalog extends the agent table that already drives sessions (`internal/creds`, keyed by a profile's `agent`). It ships in the binary and is versioned with the engine, because each entry is tested against the real CLI (spike S4) and against credential injection. For each harness it declares:

- **Headless invocation:** the command that runs the CLI non-interactively on `/icaro/prompt.md` in `/workspace`, with its permission mode fixed for unattended use (for example `claude -p` with a JSON output format).
- **Params:** name, type (`string`, `int`, `bool` or an enum), and the flag each maps to. The first set is small (`model` and `max_turns` for both CLIs) and grows one validated param at a time.
- **Result capture:** where the CLI's final answer appears. It becomes the built-in output **`result`** (capped at 1 MiB, like `/icaro/output.json`), beside any `outputs` the step declares from `/icaro/output.json`.
- **Exit mapping:** which exit codes and result fields mean failure. A failed attempt is retried under `retry` like an image step.
- **Usage:** which files in the home hold the run's token usage, so the usage parser ([ADR 0022](0022-usage-from-local-logs.md)) attributes it to the run and step before the step's home layer is discarded.

A profile can still set a custom `image`. The catalog says what must be in it (the CLI on `PATH`), and `icaro doctor` checks it.

### Running it

An agent step is one container in the run session ([ADR 0010](0010-run-sessions.md)), like an image step, with three differences:

1. **Home.** The step gets a fresh writable layer over its profile's golden seed ([ADR 0005](0005-persistent-home-egress.md), [ADR 0012](0012-egress-proxy-and-home-layers.md)), mounted as the CLI's home. The layer is discarded when the attempt ends, after usage is read. So the login is reused, and nothing a step writes (including a refreshed token) persists or reaches another step. A running agent step counts as a running overlay session of its profile, so `icaro login` refuses to rewrite the seed under it, as it does for interactive sessions.
2. **Network.** The step's container is attached to its **agent profile's** egress proxy, not the run's `workflows.profile` proxy. With `inject`, the API key stays on the host and the container gets the placeholder and base URL ([ADR 0028](0028-credential-injection.md)). A run whose agent steps use two profiles has two attachments.
3. **Image.** The profile's image is resolved to the digest pinned in the `images` table when the run starts, and that digest is recorded on the step run. Every attempt of the step, and every agent step of the run under the same profile, runs that exact image, even if the tag moves mid-run.

`/workspace`, `/icaro/input.json`, `/icaro/output.json`, the container baseline, `limits` and log redaction are those of an image step. The agent's transcript goes to the step's log like any output.

### Untrusted input

A prompt that interpolates `trigger.*` from an integration event carries text an outsider wrote, so prompt injection is the expected case, not an edge case. The design bounds what an injected agent can do; it does not try to detect injection:

- The agent can reach only its profile's egress allowlist. With `inject`, it can use the provider key through the proxy but cannot read it.
- It has the step's workspace, its own discarded home layer, and its memory scope.
- It has no daemon API, no in-container workflow tools (still [deferred](../02-decisions-risks.md#deferred-designs)), and no secrets except those the step passes explicitly in `env`.
- Validation **warns** (it does not fail) when an agent step's prompt references `trigger.*` and the workflow has an integration trigger. The warning names the profile whose egress and credentials the agent will have.

## Alternatives considered

- **Keep agents as image steps and fix the home mount.** That would close the gap with ADR 0010, but every workflow would still carry CLI-specific flags, output parsing and exit-code knowledge that nothing validates, and a profile switch would still apply to every step of a run.
- **A `harness:` field on the step, separate from the profile.** It would let a workflow pair any CLI with any profile's credentials and egress, which is a security decision ADR 0010 deliberately kept in config.
- **Harnesses as data in packs**, like integrations. Attractive for third-party CLIs, but each harness touches credential handling and S4's per-CLI findings, so the first catalog is built in. Custom invocations fit ADR 0033's presets instead.
- **A persistent home per run**, so later agent steps can resume an earlier step's conversation. Useful, but it turns step isolation into shared mutable state across steps. Left out of this ADR.

## Consequences

- A workflow can run Claude Code or Codex with the operator's login and network policy, typed params and a captured result, and with the provider key never inside the container when the profile injects it.
- Agent steps are off until the operator lists a profile in `workflows.agent_profiles`. Enabling one is an explicit decision about which credentials and egress automated, possibly untrusted, prompts get.
- The engine needs a per-step egress attachment and per-step home layers, where today a run has one attachment and steps have no home. That work also makes the ADR 0010 sentence and architecture §5 true, and the docs that make the claim are corrected when this lands.
- `agent` is a new step kind for the schema (drift-tested against [the contract](../schemas/workflow.schema.json)), the validator, the API, the CLI and the web UI's run views. `result` is a new built-in output.
- Agent runs cost money and are nondeterministic. A retry reruns the agent from scratch with a fresh home layer, but `/workspace` keeps what the failed attempt wrote, as it does for image steps.
- Implementation would be a new phase after Phase 11: the catalog, the schema and validation, per-step home layers and egress, image pinning on the step run, result and usage capture, client views, doctor checks, and docs.
