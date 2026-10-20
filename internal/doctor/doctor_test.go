package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/paths"
)

func TestDaemonCheckFixesOrphans(t *testing.T) {
	l := paths.New(filepath.Join(t.TempDir(), "r"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(l.PID(), []byte("999999"), 0o600)
	ctx := context.Background()
	reps := Run(ctx, []Check{DaemonCheck(l)}, false)
	if reps[0].Severity != Warning {
		t.Fatalf("want warning, got %+v", reps[0])
	}
	reps = Run(ctx, []Check{DaemonCheck(l)}, true)
	if reps[0].Severity != OK || reps[0].Fixed == "" {
		t.Fatalf("fix: %+v", reps[0])
	}
}

func TestPermissionsCheck(t *testing.T) {
	l := paths.New(filepath.Join(t.TempDir(), "r"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(l.ConfigDir(), "icaro.db")
	_ = os.WriteFile(f, nil, 0o644) //nolint:gosec // deliberately insecure fixture
	// User content below worktrees is not policed.
	_ = os.MkdirAll(filepath.Join(l.Worktrees(), "p", "src"), 0o755) //nolint:gosec // user content may be world-readable
	ctx := context.Background()
	reps := Run(ctx, []Check{PermissionsCheck(l)}, false)
	if reps[0].Severity != Error {
		t.Fatalf("want error: %+v", reps[0])
	}
	reps = Run(ctx, []Check{PermissionsCheck(l)}, true)
	if reps[0].Severity != OK {
		t.Fatalf("after fix: %+v", reps[0])
	}
	fi, _ := os.Stat(f)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	var buf bytes.Buffer
	Print(&buf, reps)
	if !strings.Contains(buf.String(), "fixed") || Worst(reps) != OK {
		t.Fatal(buf.String())
	}
	if Worst([]Report{{Severity: Warning}, {Severity: OK}}) != Warning {
		t.Fatal("worst")
	}
}
