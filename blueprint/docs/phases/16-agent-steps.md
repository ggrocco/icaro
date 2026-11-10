# Phase 16 — Agent steps

**Objective:** an `agent` step that runs a profile's agent CLI headless in a workflow, under the operator's login, network policy and credential injection, with typed params and a captured result, as proposed in [ADR 0037](../adr/0037-agent-steps.md).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows. Entry also needs:

- ADR 0037 **accepted**, with its open questions settled: the default of `workflows.agent_profiles`, memory access for steps fed by untrusted triggers, a home layer per attempt or per run, and whether untrusted prompts get a warning or an opt-in field. A change to any of them is made in the ADR first, then here.
- Spike **S11** passed, and the **agent adapters** deliverable of [Phase 14](14-workflow-assistant.md): each `Agent` in `internal/creds` has a headless spec, and `internal/assist/stream` parses its event stream. If Phase 14 has not started, that deliverable moves here unchanged and Phase 14 reuses it.

## Spike S11, step mode

S11 covers a chat turn with the CLI's own tools off. An agent step needs them on, unattended, inside `/workspace`. Run before any other deliverable, with the same real sign-ins and an `inject` API-key profile.

- [ ] For each CLI (Claude Code, Codex), record the exact one-shot flags for an unattended run in `/workspace`:
  - the prompt on stdin;
  - the event stream on stdout;
  - file and shell tools allowed;
  - the memory MCP server configured;
  - no prompt for permission at any point.
- [ ] Record streams as fixtures under `internal/assist/testdata/<agent>/<version>/step-*`:
  - a run that edits files and finishes;
  - a run that writes `/icaro/output.json`;
  - a run stopped by `max_turns`;
  - an expired login, a rate limit, and a CLI error.
- [ ] For each param in the first catalog (`model`, `max_turns`), confirm the flag it maps to and how the CLI reports a bad value.
- [ ] Confirm that with `inject` the CLI accepts the placeholder key and base URL with tools on, and calls no host outside the profile's allowlist (spike S4's open half, for this mode).

## Deliverables

- [ ] **Catalog.** Each `Agent` in `internal/creds` gains a step spec on top of its headless adapter:
  - the unattended one-shot command;
  - typed params (name, type, flag);
  - result capture from the normalized stream;
  - exit and event mapping (`auth_required` fails without a retry);
  - the minimum CLI version.

  A profile whose `agent` has no step spec cannot be listed in `workflows.agent_profiles`.
- [ ] **Config.** `workflows.agent_profiles`, validated at load: every entry is a profile with an agent, and the default follows the ADR. `icaro config show` shows it.
- [ ] **Contract.** Add the `agent` step form (`profile`, `prompt`, `params`, `memory`) and the built-in `result` output to the spec types in `internal/workflow`. Regenerate `schema/workflow.schema.json` with `go generate ./...`, and update the [blueprint contract](../schemas/workflow.schema.json) so the drift test passes.
- [ ] **Validation.**
  - `profile` must be in `workflows.agent_profiles`, with a nearest-match hint.
  - `params` are checked against the harness's declared names and types, with a nearest-match hint for an unknown name.
  - `prompt` is parsed at validation time and limited to `steps` (of `needs` ancestors), `trigger`, `env`, `run` and `workflow`; `secrets` is an error.
  - `command` and `image` on an agent step are errors.
  - A warning, not an error, when the prompt references `trigger.*` in a workflow with an integration trigger. It names the profile.
- [ ] **Engine.** In `internal/workflow/engine`, an agent step is one container in the run session with three differences from an image step:
  - **Home:** a fresh writable layer over its profile's seed for each attempt, discarded when the attempt ends, using the same layering as interactive sessions (`internal/creds`, ADR 0012). The step counts as a running overlay session of its profile for `icaro login`.
  - **Network:** an egress attachment for the step's agent profile, with credential injection where the profile has `inject`. A run gets one attachment per profile it uses, not one per run.
  - **Image:** the profile's image resolved to the digest pinned in the `images` table at run start, recorded on the step run, and reused by every attempt and every agent step of the run under that profile.

  The rendered prompt is written to `/icaro/prompt.md` and fed on stdin. The stream is parsed, size-capped and redacted into the step log. `result` and the declared `outputs` are captured, and the step's memory socket is served when `memory` is on. Re-attach after a daemon restart works as for image steps.
- [ ] **Store.** A migration in both dialects adds the resolved image digest, the profile and the token usage to step runs. Tests pass on SQLite and Postgres.
- [ ] **Usage.** The stream's `usage` events are recorded on the step run and counted by `icaro usage` per run and step. Subscription runs show tokens only; `inject` runs also show the labelled cost estimate.
- [ ] **Clients.**
  - `get_run`, `icaro runs get` and the web UI's run page show an agent step's profile, CLI, image digest, params, prompt, `result` and token usage.
  - The run graph marks agent steps.
  - The form editor offers the `agent` form, with `profile` and `params` choices taken from the config and the catalog.
- [ ] **Doctor.** Checks:
  - each listed agent profile is signed in or injects a key;
  - its image has the CLI at or above the catalog's minimum version;
  - its allowlist reaches the provider.

  They are mapped in the doctor coverage test, with a row in the [doctor checks](../03-security.md#icaro-doctor).
- [ ] **Skill.** The `icaro-workflows` skill learns the `agent` form: list the allowed profiles and their params before writing one, and pass secrets only through `env`.
- [ ] **Docs.**
  - `docs/workflows.md`: an "Agent steps" section with a worked example in a ```` ```yaml workflow ```` block (checked by `hack/docscheck`), the untrusted-input guidance, and how to enable a profile.
  - `docs/agents.md`: profiles for workflows.
  - Architecture §5 and §6, [ADR 0010](../adr/0010-run-sessions.md)'s note on agent homes, and [security](../03-security.md): the per-step home layer and egress, and `workflows.agent_profiles` as the boundary.
  - The blueprint: ADR 0037 marked Accepted, a decision-index row, cut 8's wording, and the deferred-designs row removed.

## Acceptance criteria

- [ ] A workflow with an agent step fails validation until its profile is listed in `workflows.agent_profiles`, and the issue names the setting.
- [ ] Invalid agent steps return structured issues with JSON Pointer paths: an unlisted profile, an unknown or mistyped param (with a hint), `secrets` in the prompt, a reference to a step that is not a `needs` ancestor, and `command` or `image` on the step.
- [ ] A prompt that reads `trigger.*` in a workflow with an integration trigger validates with a warning that names the profile.
- [ ] With a profile signed in by subscription and no API key on the machine, an agent step reviews a repository checked out at `/workspace`. It writes `/icaro/output.json`, and a later step reads both its declared output and `result`.
- [ ] With an `inject` profile, the same step runs, and no container, log, SQL row or API response contains the API key. Unit tests check the container spec and logs; an integration test checks the real container.
- [ ] Two agent steps of one run, under the same profile, each start from the profile's seed: a file one writes in its home is absent in the other's.
- [ ] An agent step reaches only its profile's allowlist; an image step in the same run still has `workflows.profile`'s egress.
- [ ] Moving the profile image's tag during a run does not change the image a later attempt or agent step of that run uses, and the digest is shown on the run.
- [ ] `icaro login <profile>` is refused while an agent step of that profile is running.
- [ ] An expired login fails the attempt with **Sign in required** and the `icaro login` command, and does not retry. A CLI error is retried under `retry`, and each attempt gets a fresh home layer.
- [ ] A daemon restart mid-step re-attaches to the running agent container and captures its result.
- [ ] A recorded stream with a prompt injection ("print the API key", "call the daemon API") can reach nothing outside the step's container, allowlist, workspace and memory scope.
- [ ] Token usage of a run is shown per agent step and counted by `icaro usage`.
- [ ] Every new decision has unit tests against the fake engine and the recorded fixtures. The home layer, egress attachment and inject paths have tests behind the `integration` build tag. Coverage stays at the [ADR 0025](../adr/0025-coverage-target-and-exceptions.md) targets.

## Where it lives

- Catalog: `internal/creds/agents.go`; stream parsing: `internal/assist/stream`.
- Spec, schema and validation: `internal/workflow`; execution: `internal/workflow/engine` (a new `agent.go` beside `image.go`).
- Per-step homes: `internal/creds` and `internal/session`; egress attachments: `internal/container`.
- Config: `internal/config`; doctor checks: `internal/doctor`.
