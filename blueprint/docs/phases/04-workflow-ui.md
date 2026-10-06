# Phase 4 — Workflow UI and agent authoring

**Objective:** React editor and observability for humans; MCP tools and the authoring skill for agents ([architecture §10](../01-architecture.md#10-agent-interface)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Create a listing, a details view, and an SSE log stream.
- [ ] Add the form editor first, plus a YAML editor bound to the JSON Schema (autocomplete, inline issues); drag/drop canvas later.
- [ ] Generate canonical JSON and validate before saving.
- [ ] Display the run as a graph from its pinned workflow version, so pending steps are visible: status, attempts, outputs, and live logs per step; re-run; version history with diff.
- [ ] Add a deliveries view with payloads and **Redeliver**.
- [ ] Render `uses:` steps as real forms from the action's typed input declaration; pick triggers from installed integrations' events.
- [ ] Add an integration manifest editor: edits are validated, written to the pack, and **committed to the pack's git repo** with a structured message; push stays a manual button.
- [ ] Add accessibility and error/empty states.
- [ ] Expose the authoring MCP tools (stdio + streamable HTTP) and ship the `icaro-workflows` skill with `icaro skill install`.

## Acceptance criteria

- [ ] Editing in the form produces compatible JSON.
- [ ] A canvas round-trip does not lose unknown fields.
- [ ] Saving a manifest edit in the UI produces a commit in the pack repo; an invalid edit is rejected before any write.
- [ ] The UI updates status without aggressive polling.
- [ ] An E2E test covers create, run, and cancel.
- [ ] An MCP-capable agent using the skill goes from `get_workflow_schema` through `validate_workflow` (fixing a deliberate typo from its issue path), `create_workflow`, `run_workflow`, and `get_run`.
- [ ] No MCP tool returns a secret value or deletes anything.
