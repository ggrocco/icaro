# Phase 4 — Workflow UI

**Objective:** React editor and observability.

**Depends on:** see the [roadmap](../04-roadmap.md#dependencies), which also defines the entry, test, and exit rules every phase follows.

## Deliverables

- [ ] Create a listing, a details view, and an SSE log stream.
- [ ] Add the form editor first; drag/drop canvas later.
- [ ] Generate canonical JSON and validate before saving.
- [ ] Display the DAG, status, and attempts per step.
- [ ] Render `uses:` steps as real forms from the action's typed input declaration; pick triggers from installed integrations' events.
- [ ] Add an integration manifest editor: edits are validated, written to the pack, and **committed to the pack's git repo** with a structured message; push stays a manual button.
- [ ] Add accessibility and error/empty states.

## Acceptance criteria

- [ ] Editing in the form produces compatible JSON.
- [ ] A canvas round-trip does not lose unknown fields.
- [ ] Saving a manifest edit in the UI produces a commit in the pack repo; an invalid edit is rejected before any write.
- [ ] The UI updates status without aggressive polling.
- [ ] An E2E test covers create, run, and cancel.
