package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/creds"
	"icaro/internal/session"
	"icaro/internal/store"
)

func TestSessionRoutes(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	decode := func(body string) session.Session {
		t.Helper()
		var s session.Session
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return s
	}
	if code, body := f.call("GET", "/api/v1/sessions", nil); code != 200 || !strings.Contains(body, `"sessions": []`) {
		t.Fatalf("empty list: %d %s", code, body)
	}
	if code, body := f.call("POST", "/api/v1/sessions", map[string]any{}); code != 400 || !strings.Contains(body, "repo_path is required") {
		t.Fatalf("no repo: %d %s", code, body)
	}
	if code, _ := f.call("POST", "/api/v1/sessions", []byte(`{"repo_path":`)); code != 400 {
		t.Fatalf("bad JSON: %d", code)
	}
	if code, body := f.call("POST", "/api/v1/sessions", map[string]any{"repo_path": t.TempDir()}); code != 400 {
		t.Fatalf("not a repository: %d %s", code, body)
	}

	code, body := f.call("POST", "/api/v1/sessions", map[string]any{"repo_path": f.gitRepo(), "name": "fix login"})
	s := decode(body)
	if code != http.StatusCreated || s.Status != session.StatusRunning || s.Branch != "icaro/fix-login" {
		t.Fatalf("create: %d %s", code, body)
	}
	// Ensure returns the running session for the same profile and name.
	code, body = f.call("POST", "/api/v1/sessions/ensure", map[string]any{"repo_path": f.gitRepo(), "name": "fix login"})
	if code != http.StatusCreated || decode(body).ID != s.ID {
		t.Fatalf("ensure: %d %s", code, body)
	}
	if code, body := f.call("GET", "/api/v1/sessions?status=running&kind=agent", nil); code != 200 || !strings.Contains(body, s.ID) {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, body := f.call("GET", "/api/v1/projects", nil); code != 200 || !strings.Contains(body, `"root_path"`) {
		t.Fatalf("projects: %d %s", code, body)
	}
	if code, body := f.call("GET", "/api/v1/sessions/"+s.ID, nil); code != 200 || decode(body).WorktreePath != s.WorktreePath {
		t.Fatalf("get: %d %s", code, body)
	}
	code, body = f.call("GET", "/api/v1/sessions/"+s.ID+"/exec", nil)
	if code != 200 || !strings.Contains(body, `"user": "1234:1234"`) || !strings.Contains(body, `"agent": "claude"`) {
		t.Fatalf("exec: %d %s", code, body)
	}
	if code, body := f.call("POST", "/api/v1/sessions/"+s.ID+"/stop", nil); code != 200 || decode(body).Status != session.StatusStopped {
		t.Fatalf("stop: %d %s", code, body)
	}
	// A stopped session cannot be exec'd into: the manager's ErrInvalid
	// becomes a 400 with its message.
	if code, body := f.call("GET", "/api/v1/sessions/"+s.ID+"/exec", nil); code != 400 || !strings.Contains(body, "not running") {
		t.Fatalf("exec stopped: %d %s", code, body)
	}
	if code, body := f.call("POST", "/api/v1/sessions/"+s.ID+"/start", nil); code != 200 || decode(body).Status != session.StatusRunning {
		t.Fatalf("start: %d %s", code, body)
	}
	if code, _ := f.call("DELETE", "/api/v1/sessions/"+s.ID+"?keep_worktree=true", nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if _, err := filepath.EvalSymlinks(s.WorktreePath); err != nil {
		t.Fatalf("keep_worktree removed the worktree: %v", err)
	}
	for _, p := range []string{"", "/exec"} {
		if code, _ := f.call("GET", "/api/v1/sessions/"+s.ID+p, nil); code != 404 {
			t.Fatalf("after remove %s: %d", p, code)
		}
	}
	for _, p := range []string{"/stop", "/start"} {
		if code, _ := f.call("POST", "/api/v1/sessions/"+s.ID+p, nil); code != 404 {
			t.Fatalf("after remove %s: %d", p, code)
		}
	}
	if code, _ := f.call("DELETE", "/api/v1/sessions/"+s.ID, nil); code != 404 {
		t.Fatalf("remove twice: %d", code)
	}
	if code, _ := f.call("POST", "/api/v1/sessions/ensure", map[string]any{"repo_path": t.TempDir()}); code != 400 {
		t.Fatalf("ensure outside a repository: %d", code)
	}
}

func TestProfileLoginRoute(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	code, body := f.call("POST", "/api/v1/profiles/claude/login", nil)
	var li LoginInfo
	_ = json.Unmarshal([]byte(body), &li)
	homes := &creds.Homes{Layout: f.cfg.Layout()}
	if code != 200 || li.Image == "" || !strings.HasSuffix(li.Image, "@sha256:abc") || li.SeedDir != homes.SeedDir("claude") ||
		li.User != "1234:1234" || strings.Join(li.Command, " ") != "claude" || li.Egress.Network == "" {
		t.Fatalf("login: %d %s", code, body)
	}
	if code, _ := f.call("POST", "/api/v1/profiles/nope/login", nil); code != 404 {
		t.Fatalf("unknown profile: %d", code)
	}
	if code, body := f.call("POST", "/api/v1/profiles/shell/login", nil); code != 400 || !strings.Contains(body, "no agent to log in to") {
		t.Fatalf("shell profile: %d %s", code, body)
	}
	// A login token over TCP needs admin scope.
	if code, _ := doReq(t, f.tcp.Client(), "POST", f.tcp.URL+"/api/v1/profiles/claude/login", nil, nil); code != 401 {
		t.Fatalf("tcp: %d", code)
	}
	// A session that will read the seed blocks the login, unless forced.
	proj, err := f.st.UpsertProject(f.ctx, &store.Project{Name: "p", RootPath: f.repo})
	if err != nil {
		t.Fatal(err)
	}
	row := &store.SessionRow{Kind: session.KindAgent, ProjectID: proj.ID, Profile: "claude", Branch: "b", WorktreePath: "/w", Status: session.StatusQueued}
	if err := f.st.CreateSession(f.ctx, row); err != nil {
		t.Fatal(err)
	}
	if code, body := f.call("POST", "/api/v1/profiles/claude/login", nil); code != 409 || !strings.Contains(body, row.ID) {
		t.Fatalf("busy profile: %d %s", code, body)
	}
	if code, body := f.call("POST", "/api/v1/profiles/claude/login?force=true", nil); code != 200 {
		t.Fatalf("forced login: %d %s", code, body)
	}
}

func TestProfileLoginDependencyFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps func(d *SessionDeps)
	}{
		{"image", func(d *SessionDeps) { d.Images = fakeImages{err: errBoom} }},
		{"egress", func(d *SessionDeps) { d.Egress = fakeEgress{err: errBoom} }},
		// The seed directory cannot be created under a regular file.
		{"seed", func(d *SessionDeps) { d.Homes = &creds.Homes{Layout: blockedLayout(t)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFull(t)
			d := SessionDeps{Egress: fakeEgress{}, Images: fakeImages{}, Homes: &creds.Homes{Layout: f.cfg.Layout()}, Config: f.cfg}
			tc.deps(&d)
			s := New(f.svc, quietLog())
			s.RegisterSessions(d)
			c := serveUnix(t, s.Handler())
			code, body := doReq(t, c, "POST", "http://icaro/api/v1/profiles/claude/login", nil, nil)
			if code != 500 || errBody(t, body).Error != "internal error" {
				t.Fatalf("%d %s", code, body)
			}
		})
	}
}
