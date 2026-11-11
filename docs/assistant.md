# Workflow assistant

The workflow assistant is a chat beside the workflow editor. You describe what you want; it reads the schema, your integrations, secret names, workflows and runs, validates a draft, and proposes it. You see each proposal as a diff, accept or discard it, and save it yourself. When it wants to run a workflow, it asks you first.

It runs **your own agent CLI** (Claude Code or Codex) under the login of one of your profiles. A Claude Pro/Max or ChatGPT subscription therefore works without an API key: Ícaro starts the vendor's CLI headless in a container, the same way it runs agent steps, and never reads or forwards your login token ([ADR 0035](../blueprint/docs/adr/0035-workflow-assistant-chat.md)).

## Set up

The assistant uses the first profile, by name, whose `agent` is `claude`, or else `codex`. To pick one explicitly:

```yaml
# ~/.icaro/config/icaro.yaml
assistant:
  profile: claude          # a profile whose agent is claude or codex
  max_active: 2            # assistant containers running at once; more chats wait
  idle_timeout: 15m        # stop a chat's container after this long without a message
  turn_timeout: 10m        # the longest one reply may take
  approval_timeout: 5m     # how long a run request waits for you
```

### With a subscription

Sign the profile in once, as for agent sessions:

```sh
icaro login claude         # or: icaro login codex
icaro doctor               # the "workflow assistant" check confirms it
```

The profile's egress allowlist must reach the provider. The built-in `claude` and `codex` profiles already do.

### With an API key

Give the profile an injected key ([credential injection](sessions.md#credential-injection-api-keys)); the key stays on the host and the assistant works the same way:

```yaml
profiles:
  claude:
    inject: { anthropic: anthropic-key }
```

## Use it

- **Web UI.** Open a workflow, select **Edit**, then **Ask the assistant**. Or start from **Workflows → New with assistant**, or from the **Assistant** page, which lists every chat. The desktop app's tray has **Ask the assistant**.
- **CLI.** `icaro assist new`, `icaro assist send CHAT "message"` (prints the reply as it streams), `icaro assist show CHAT`, `icaro assist approve|decline CHAT APPROVAL`, `icaro assist cancel CHAT`, `icaro assist rm CHAT --yes`. See the [CLI reference](cli.md).

Each message is one turn. The turns of a chat run one after the other; **Cancel** stops the running one and drops the queued ones.

### Proposals

The assistant reads what is in your editor (`get_draft`) and writes proposals (`propose_workflow`). A proposal is validated and shown as a diff against your editor with its issues. **Accept into the editor** only changes the editor; save it with **Validate & save**. A version saved with a chat open is recorded as `<you> via assistant <chat>` in the workflow's history. **Discard** keeps your document, and the assistant sees your version next time it reads the draft. Your own edits reach the assistant too, as the chat's draft.

### Runs

The assistant can ask to start a manual run of a **saved** workflow. You get a card with the workflow, its version and the trigger payload. Nothing runs until you **Approve run**. **Decline**, or no answer within `assistant.approval_timeout`, tells the assistant no.

## What it can and cannot do

| It can | It cannot |
|---|---|
| Read the workflow schema, integrations and their typed inputs, secret **names** and host bindings, workflows, and runs with a log tail | Save, change or delete a workflow; you apply its proposals |
| Validate drafts and propose them | See a secret's value |
| Ask you to run a saved workflow | Start a run without your approval |
| Read the project's memory, when the chat has a project | Write memory |
| | Use a shell, files or the web: its CLI runs with only the `icaro` tools |

Run logs, integration manifests and memory can contain text written by someone else. The assistant is told to treat them as data, but a prompt injection could still steer a proposal. That is why every change goes through your diff and every run through your approval.

## States you may see

- **Waiting for a free assistant slot.** More chats are active than `assistant.max_active`; the turn starts when one stops.
- **Sign in required.** The profile's CLI is not signed in, or its login expired. Run `icaro login <profile>`, then **Retry**.
- **Usage limit reached.** Your subscription's usage window is spent; the reset time is shown when the CLI reports it. Ícaro never retries on its own, so nothing spends your window in the background.
- **The reply was interrupted.** The daemon restarted during the turn. **Retry** sends the same message again; the conversation continues where it was.

## Usage and cost

Each reply shows the tokens the CLI reported. A subscription reply shows tokens only. An API-key profile also shows the CLI's cost figure, labelled an estimate. The assistant's transcripts sit in the profile home, so `icaro usage` counts them too.

## Where things are

- Chats, messages, drafts and run requests: the database (`chats`, `chat_messages`, `chat_drafts`, `chat_approvals`).
- Each chat's event log, secret-redacted as it is written: `~/.icaro/logs/assist/<chat>.jsonl`.
- Each chat's scratch workspace: `~/.icaro/assist/<chat>/workspace`. Its container is a session of kind `assist` (`icaro session list --kind assist`).

Deleting a chat removes its container, workspace, rows and log.

## Before relying on it

The assistant is built against stream formats modeled on the CLIs' documentation. Spike S11 in the [feasibility gate](../blueprint/docs/02-decisions-risks.md#feasibility-gate) records real streams from signed-in CLIs before release. Until then, a new CLI version may change a flag or an event: the chat then shows an error rather than a wrong answer. Whether driving a subscription CLI headless suits your plan is between you and the provider; Ícaro only runs the official CLI, unchanged, under your login.
