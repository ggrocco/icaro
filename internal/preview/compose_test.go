package preview

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseComposeForms(t *testing.T) {
	doc := `services:
  web:
    image: x
    privileged: "true"
    labels: {1: one}
    ports: ["abc", "8080/tcp"]
    volumes:
      - {source: data, target: /data}
volumes:
  data:
    driver_opts: {type: none, o: bind, device: ./data}
`
	p, err := ParseCompose([]byte(doc), "/wt")
	if err != nil {
		t.Fatal(err)
	}
	s := p.Services["web"]
	// A quoted boolean still counts: "privileged: 'true'" must not slip
	// past the lint.
	if !s.Privileged || s.Labels["1"] != "one" {
		t.Fatalf("%+v", s)
	}
	if len(s.Ports) != 1 || s.Ports[0] != 8080 {
		t.Fatalf("an unparsable port is ignored: %v", s.Ports)
	}
	if len(s.Mounts) != 1 || s.Mounts[0].Type != "volume" {
		t.Fatalf("a long-form volume without type is a named volume: %+v", s.Mounts)
	}
	if v := p.Volumes["data"]; !v.Bind || v.Device != "/wt/data" {
		t.Fatalf("relative bind devices resolve against the worktree: %+v", v)
	}

	if _, err := ParseCompose([]byte("services:\n  web:\n    volumes: [\"\"]\n"), "/wt"); err == nil || !strings.Contains(err.Error(), `service "web" volume 0`) {
		t.Fatal(err)
	}
}

func TestScalarConversions(t *testing.T) {
	for v, want := range map[any]string{nil: "", "s": "s", json.Number("12"): "12", 1.5: "1.5", 7: "7"} {
		if got := asString(v); got != want {
			t.Fatalf("asString(%#v) = %q", v, got)
		}
	}
	for v, want := range map[any]bool{true: true, "true": true, "no": false, 1: false, nil: false} {
		if asBool(v) != want {
			t.Fatalf("asBool(%#v)", v)
		}
	}
	if atoi(" 80 ") != 80 || atoi("x") != 0 {
		t.Fatal("atoi")
	}
}

func TestFindingStringAndEmptyAllowRoots(t *testing.T) {
	f := Finding{ID: "web/x", Field: "services.web.x", Reason: "why"}
	if f.String() != "web/x: services.web.x — why (allow with: --finding web/x)" {
		t.Fatal(f.String())
	}
	// Empty or "." roots never allow anything (they would match relative
	// junk, not the worktree).
	_, inWT := bindProblem("/wt/a", resolveRoots([]string{"", ".", "/wt"}))
	_, outside := bindProblem("/x", resolveRoots([]string{"", "."}))
	if inWT != "" || outside == "" {
		t.Fatal("bindProblem roots")
	}
}

func TestWatcherLogsFailuresAndSkipsUnhashable(t *testing.T) {
	root := t.TempDir()
	good := worktree(t, root, "good", webCompose)
	bad := worktree(t, root, "bad", webCompose)
	_ = os.Symlink(".env", filepath.Join(bad, ".env")) // cannot be hashed
	if got := Scan([]string{root}); len(got) != 1 || got[norm(good)] == "" {
		t.Fatalf("an unhashable candidate is skipped: %v", got)
	}

	var logs strings.Builder
	var synced, removed int
	w := newWatcher([]string{root}, 0,
		func(context.Context, string) error { synced++; return errors.New("sync failed") },
		func(context.Context, string) error { removed++; return errors.New("remove failed") },
		slog.New(slog.NewTextHandler(&logs, nil)))
	ctx := context.Background()
	w.tick(ctx) // primes the debounce
	w.tick(ctx)
	if synced != 1 || !strings.Contains(logs.String(), "sync failed") {
		t.Fatalf("synced=%d %s", synced, logs.String())
	}
	_ = os.RemoveAll(good)
	w.tick(ctx) // vanished once: wait
	w.tick(ctx)
	if removed != 1 || !strings.Contains(logs.String(), "remove failed") {
		t.Fatalf("removed=%d %s", removed, logs.String())
	}
}
