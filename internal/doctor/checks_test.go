package doctor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/daemon"
	"icaro/internal/paths"
)

// shortLayout keeps the socket path under the macOS 104-byte limit.
func shortLayout(t *testing.T) paths.Layout {
	t.Helper()
	dir, err := os.MkdirTemp("", "dr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return paths.New(filepath.Join(dir, "r"))
}

func TestDaemonCheckStates(t *testing.T) {
	ctx := context.Background()
	l := shortLayout(t)

	if r := DaemonCheck(l).Run(ctx); r.Severity != OK || !strings.Contains(r.Detail, "no daemon") {
		t.Fatalf("idle: %+v", r)
	}

	inst, err := daemon.Acquire(l)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close() }()
	// Lock held but nothing answers on the socket.
	if r := DaemonCheck(l).Run(ctx); r.Severity != Error || !strings.Contains(r.Detail, "does not answer") {
		t.Fatalf("hung: %+v", r)
	}

	ln, err := inst.ListenUnix()
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	if r := DaemonCheck(l).Run(ctx); r.Severity != OK || !strings.Contains(r.Detail, "running") {
		t.Fatalf("running: %+v", r)
	}
}

func TestDaemonCheckProbeError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	l := paths.New(root)
	// The run directory is a file, so the lock cannot be probed.
	if err := os.WriteFile(filepath.Dir(l.Lock()), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := DaemonCheck(l).Run(context.Background()); r.Severity != Error {
		t.Fatalf("probe error: %+v", r)
	}
}

func TestRunFixFailureAndPrint(t *testing.T) {
	ctx := context.Background()
	calls := 0
	failing := Func("broken", func(context.Context) Result {
		calls++
		return Result{Severity: Error, Detail: "bad", Fix: func(context.Context) (string, error) {
			return "", errors.New("no permission")
		}}
	})
	skipped := Func("n/a", func(context.Context) Result { return Result{Severity: Skipped, Detail: "not on this OS"} })
	// OK results never run their fix, even with fix on.
	okWithFix := Func("fine", func(context.Context) Result {
		return Result{Severity: OK, Detail: "ok", Fix: func(context.Context) (string, error) {
			t.Fatal("fix must not run for OK")
			return "", nil
		}}
	})
	reps := Run(ctx, []Check{failing, skipped, okWithFix}, true)
	if calls != 1 {
		t.Fatalf("a failed fix must not re-run the check: %d", calls)
	}
	if reps[0].FixError != "no permission" || reps[0].Fixed != "" || reps[0].Severity != Error {
		t.Fatalf("%+v", reps[0])
	}
	if Worst(reps) != Error {
		t.Fatal("worst must be error")
	}
	var buf bytes.Buffer
	Print(&buf, reps)
	out := buf.String()
	for _, want := range []string{"[FAIL] broken: bad", "fix failed: no permission", "[skip] n/a", "[ok  ] fine"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if Worst(nil) != OK {
		t.Fatal("no reports is OK")
	}
}

func TestPermissionsCheckEdges(t *testing.T) {
	ctx := context.Background()
	missing := paths.New(filepath.Join(t.TempDir(), "absent"))
	if r := PermissionsCheck(missing).Run(ctx); r.Severity != OK || !strings.Contains(r.Detail, "not created") {
		t.Fatalf("missing root: %+v", r)
	}

	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := PermissionsCheck(paths.New(filepath.Join(file, "r"))).Run(ctx); r.Severity != Error {
		t.Fatalf("unreadable root: %+v", r)
	}

	l := shortLayout(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Symlinks and sockets are not policed; a world-readable directory is
	// tightened to 0700.
	if err := os.Symlink("/etc/hosts", filepath.Join(l.ConfigDir(), "link")); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(filepath.Dir(l.Socket()), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_ = os.Chmod(filepath.Join(filepath.Dir(l.Socket()), "s"), 0o777) //nolint:gosec // sockets are exempt
	open := filepath.Join(l.ConfigDir(), "shared")
	if err := os.Mkdir(open, 0o755); err != nil { //nolint:gosec // deliberately insecure fixture
		t.Fatal(err)
	}
	gone := filepath.Join(l.ConfigDir(), "gone")
	if err := os.WriteFile(gone, nil, 0o644); err != nil { //nolint:gosec // deliberately insecure fixture
		t.Fatal(err)
	}
	r := PermissionsCheck(l).Run(ctx)
	if r.Severity != Error || !strings.Contains(r.Detail, "2 path(s)") {
		t.Fatalf("want exactly the dir and the file: %+v", r)
	}
	// A path removed between the check and the fix is skipped.
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	did, err := r.Fix(ctx)
	if err != nil || !strings.Contains(did, "tightened 2") {
		t.Fatalf("fix: %q %v", did, err)
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
}
