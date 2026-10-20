# Phase 4 — Workflow UI and agent authoring

**Objective:** React editor and observability for humans; MCP tools and the authoring skill for agents ([architecture §10](../01-architecture.md#10-agent-interface)).

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [x] Create a listing, a details view, and an SSE log stream.
- [x] Add the form editor first, plus a YAML editor bound to the JSON Schema (autocomplete, inline issues); drag/drop canvas later. *(The graph is read-only; drag/drop editing is deferred as planned.)*
- [x] Generate canonical JSON and validate before saving.
- [x] Display the run as a graph from its pinned workflow version, so pending steps are visible: status, attempts, outputs, and live logs per step; re-run; version history with diff.
- [x] Add a deliveries view with payloads and **Redeliver**.
- [x] Render `uses:` steps as real forms from the action's typed input declaration; pick triggers from installed integrations' events.
- [x] Add an integration manifest editor: edits are validated, written to the pack, and **committed to the pack's git repo** with a structured message; push stays a manual button.
- [x] Add accessibility and error/empty states.
- [x] Expose the authoring MCP tools (stdio + streamable HTTP) and ship the `icaro-workflows` skill with `icaro skill install`.

## Acceptance criteria

- [x] Editing in the form produces compatible JSON.
- [x] A canvas round-trip does not lose unknown fields. *(Covered for the form editor, which edits the YAML document in place; `web/src/utils.test.ts`.)*
- [x] Saving a manifest edit in the UI produces a commit in the pack repo; an invalid edit is rejected before any write.
- [x] The UI updates status without aggressive polling.
- [x] An E2E test covers create, run, and cancel.
- [x] An MCP-capable agent using the skill goes from `get_workflow_schema` through `validate_workflow` (fixing a deliberate typo from its issue path), `create_workflow`, `run_workflow`, and `get_run`.
- [x] No MCP tool returns a secret value or deletes anything.

## Where it lives

- UI: `web/` (React + Vite, embedded by `web/embed.go`, served at `/ui/`); E2E in `web/e2e/` (Playwright, real daemon and Docker).
- Browser access: `icaro ui` and cookie sessions ([ADR 0023](../adr/0023-browser-access.md)); `internal/api/ui.go`, `internal/app/ui.go`.
- Authoring tools: `internal/authoring/` on the daemon MCP server (`/mcp`, `icaro mcp`); skill in `skills/icaro-workflows/`, installed by `icaro skill install`.
