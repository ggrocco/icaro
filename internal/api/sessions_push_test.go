package api

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"icaro/internal/session"
)

func TestSessionPushRoute(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	repo := f.gitRepo()
	bare := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "--bare", bare}, {"-C", repo, "remote", "add", "local", bare}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // test fixture
			t.Fatalf("%v %s", err, out)
		}
	}
	code, body := f.call("POST", "/api/v1/sessions", map[string]any{"repo_path": repo, "name": "p"})
	var s session.Session
	if err := json.Unmarshal([]byte(body), &s); err != nil || code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	push := "/api/v1/sessions/" + s.ID + "/push"
	if code, body := f.call("POST", push, []byte(`{"remote":`)); code != 400 {
		t.Fatalf("bad JSON: %d %s", code, body)
	}
	if code, body := f.call("POST", push, map[string]any{"remote": "https://evil.example/r.git"}); code != 400 || !strings.Contains(body, "not a URL") {
		t.Fatalf("URL remote: %d %s", code, body)
	}
	if code, body := f.call("POST", push, nil); code != 400 || !strings.Contains(body, "no remote named") {
		t.Fatalf("no origin: %d %s", code, body)
	}
	// git refuses the file transport: the push failure is a 502 with
	// git's reason, not an internal error.
	if code, body := f.call("POST", push, map[string]any{"remote": "local", "force_with_lease": true}); code != http.StatusBadGateway || !strings.Contains(body, "file") {
		t.Fatalf("file remote: %d %s", code, body)
	}
	if code, _ := f.call("POST", "/api/v1/sessions/nope/push", nil); code != 404 {
		t.Fatalf("unknown session: %d", code)
	}
}
