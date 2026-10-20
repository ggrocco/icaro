package integration

// ActionInputSchemas returns one JSON Schema per loaded action, keyed by
// <integration>.<action> (the MCP action-schemas resource).
func (h *Hub) ActionInputSchemas() map[string]map[string]any {
	snap := h.registry.Snapshot()
	out := map[string]map[string]any{}
	for _, ref := range snap.ActionRefs() {
		if s, ok := snap.ActionInputSchema(ref); ok {
			out[ref] = s
		}
	}
	return out
}
