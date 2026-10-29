package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestStoreFailureMapping closes the database under a running daemon:
// every route must answer with an error status, and a 500 must never
// leak the underlying error. The remaining 400s are requests that are
// wrong whatever the store does (no compose file, a weak secret value).
func TestStoreFailureMapping(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	dir := t.TempDir()
	_ = f.st.Close()
	internal := http.StatusInternalServerError
	for _, tc := range []struct {
		method, path string
		body         any
		status       int
	}{
		{"GET", "/api/v1/workflows", nil, internal},
		{"POST", "/api/v1/workflows", wfYAML, internal},
		{"GET", "/api/v1/workflows/hello", nil, internal},
		{"DELETE", "/api/v1/workflows/hello", nil, internal},
		{"POST", "/api/v1/workflows/hello/enable", nil, internal},
		{"GET", "/api/v1/workflows/hello/versions", nil, internal},
		{"GET", "/api/v1/workflows/hello/versions/1", nil, internal},
		{"POST", "/api/v1/workflows/hello/runs", map[string]any{}, internal},
		{"GET", "/api/v1/runs", nil, internal},
		{"GET", "/api/v1/runs/r1", nil, internal},
		{"POST", "/api/v1/runs/r1/cancel", nil, internal},
		{"POST", "/api/v1/runs/r1/rerun", nil, internal},
		{"GET", "/api/v1/runs/r1/events", nil, internal},
		{"GET", "/api/v1/runs/r1/steps/s/logs", nil, internal},

		{"GET", "/api/v1/projects", nil, internal},
		{"GET", "/api/v1/sessions", nil, internal},
		{"POST", "/api/v1/sessions", map[string]any{"repo_path": f.gitRepo()}, internal},
		{"POST", "/api/v1/sessions/ensure", map[string]any{"repo_path": f.gitRepo()}, internal},
		{"GET", "/api/v1/sessions/s1", nil, internal},
		{"GET", "/api/v1/sessions/s1/exec", nil, internal},
		{"POST", "/api/v1/sessions/s1/stop", nil, internal},
		{"POST", "/api/v1/sessions/s1/start", nil, internal},
		{"DELETE", "/api/v1/sessions/s1", nil, internal},
		{"GET", "/api/v1/secrets", nil, internal},
		{"PUT", "/api/v1/secrets/gh", map[string]any{"value": "ghp_123456"}, http.StatusBadRequest},
		{"DELETE", "/api/v1/secrets/gh", nil, internal},

		{"GET", "/api/v1/previews", nil, internal},
		{"GET", "/api/v1/previews/p1", nil, internal},
		{"GET", "/api/v1/previews/p1/logs", nil, internal},
		{"POST", "/api/v1/previews/up", map[string]any{"path": dir}, http.StatusBadRequest},
		{"POST", "/api/v1/previews/down", map[string]any{"path": dir}, internal},
		{"POST", "/api/v1/previews/remove", map[string]any{"path": dir}, internal},
		{"POST", "/api/v1/previews/lint", map[string]any{"path": dir}, http.StatusBadRequest},
		{"POST", "/api/v1/previews/allow", map[string]any{"path": dir, "findings": []string{"web/privileged"}, "confirm": true}, http.StatusBadRequest},

		{"POST", "/api/v1/memory", map[string]any{"project": "pa", "kind": "task", "title": "x"}, internal},
		{"GET", "/api/v1/memory?project=pa&q=release&kind=task,decision&since=2026-01-01T00:00:00Z", nil, internal},
		{"GET", "/api/v1/memory/handoffs?project=pa", nil, internal},
		{"GET", "/api/v1/memory/m1?project=pa", nil, internal},
		{"POST", "/api/v1/memory/m1/accept", map[string]any{"project": "pa"}, internal},
		{"POST", "/api/v1/memory/ingest", map[string]any{"project": "pa", "path": dir}, internal},
		{"POST", "/api/v1/memory/import", map[string]any{"project": "pa", "format": "ai-memory", "data": `[{"id":"z","title":"t"}]`}, internal},

		{"GET", "/api/v1/credentials", nil, internal},
		{"POST", "/api/v1/credentials", map[string]any{"integration": "github", "name": "token", "value": "ghp_abcdefghijklmnopqrstuvwxyz012345"}, internal},
		{"DELETE", "/api/v1/credentials/github.token", nil, internal},
		{"GET", "/api/v1/deliveries", nil, internal},
		{"GET", "/api/v1/deliveries/d1", nil, internal},
		{"POST", "/api/v1/deliveries/d1/redeliver", nil, internal},
		{"GET", "/api/v1/webhook-tokens", nil, internal},
		{"POST", "/api/v1/webhook-tokens/triage/rotate", nil, internal},

		{"POST", "/api/v1/ui/link", nil, internal},
	} {
		code, body := f.call(tc.method, tc.path, tc.body)
		if code != tc.status {
			t.Errorf("%s %s = %d, want %d: %s", tc.method, tc.path, code, tc.status, body)
			continue
		}
		if code == internal && (errBody(t, body).Error != "internal error" || strings.Contains(body, "database is closed")) {
			t.Errorf("%s %s leaked: %s", tc.method, tc.path, body)
		}
	}
}
