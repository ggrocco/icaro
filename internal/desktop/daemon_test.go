package desktop

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"icaro/internal/paths"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "icaro.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenResolvesLikeTheCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.RootEnv, "")
	d, err := Open(Options{ConfigPath: writeConfig(t, ""), Home: home, Icaro: "/x/icaro"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Remote || d.Layout.Root != home || d.icaro != "/x/icaro" || os.Getenv(paths.RootEnv) != home {
		t.Fatalf("%+v", d)
	}
	if !slices.Equal(d.args[2:], []string{"--home", home}) || d.args[0] != "--config" {
		t.Fatalf("the daemon gets the app's flags: %v", d.args)
	}

	d, err = Open(Options{ConfigPath: writeConfig(t, "daemon:\n  url: http://127.0.0.1:9\n  token: tok\n")})
	if err != nil || !d.Remote || d.Client.Token != "tok" || len(d.args) != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := Open(Options{ConfigPath: writeConfig(t, "daemon:\n  url: http://127.0.0.1:9\n")}); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatal(err)
	}
	if _, err := Open(Options{ConfigPath: writeConfig(t, "daemon: [")}); err == nil {
		t.Fatal("a broken config is an error")
	}
	if _, err := Open(Options{ConfigPath: writeConfig(t, "workflows:\n  concurrency: 0\n")}); err == nil {
		t.Fatal("an invalid config is an error")
	}
}

// The acceptance criterion: opening the app while the daemon runs does
// not start a second one.
func TestEnsureAttachesBeforeStarting(t *testing.T) {
	f, srv := newFakeAPI(t)
	st := newStarter()
	d := localDaemon(f, srv, st)
	if err := d.Ensure(context.Background()); err != nil || st.count() != 0 {
		t.Fatalf("a running daemon is attached, not started: %v %d", err, st.count())
	}

	f.set(func(f *fakeAPI) { f.down = true })
	st.up = func() { f.down = false }
	if err := d.Ensure(context.Background()); err != nil || st.count() != 1 {
		t.Fatal(err, st.count())
	}
	if !slices.Equal(st.calls[0], []string{"/opt/icaro/bin/icaro", "--home", "/h"}) {
		t.Fatalf("started with the app's flags: %v", st.calls[0])
	}
	if err := d.Ensure(context.Background()); err != nil || st.count() != 1 {
		t.Fatal("once it answers, it is not started again")
	}

	f.set(func(f *fakeAPI) { f.down = true })
	st.err = errors.New("daemon exited during startup")
	if err := d.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatal(err)
	}

	// Without an explicit binary, it is searched for.
	d.icaro = ""
	t.Setenv(IcaroEnv, "/nonexistent/icaro")
	if err := d.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), IcaroEnv) {
		t.Fatal(err)
	}
}

func TestRemoteDaemonIsNeverStarted(t *testing.T) {
	f, srv := newFakeAPI(t)
	d := remoteDaemon(srv)
	if err := d.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *fakeAPI) { f.down = true })
	if err := d.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), srv.URL) {
		t.Fatal(err)
	}
}

// The real socket path: Open's client and health check reach a daemon
// listening on the layout's socket.
func TestOpenTalksToTheSocket(t *testing.T) {
	home, err := os.MkdirTemp("", "icd") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	d, err := Open(Options{ConfigPath: writeConfig(t, ""), Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Healthy(context.Background()); err == nil {
		t.Fatal("nothing listens yet")
	}
	if err := d.Layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", d.Layout.Socket())
	if err != nil {
		t.Fatal(err)
	}
	f, _ := newFakeAPI(t)
	srv := &http.Server{Handler: f} //nolint:gosec // test server
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if err := d.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := Fetch(context.Background(), d); !st.Reachable || st.Err != "" {
		t.Fatalf("%+v", st)
	}
}

func TestFindIcaro(t *testing.T) {
	// Resolved: macOS temp dirs sit under a symlink, and FindIcaro
	// resolves the app's path.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := func(name string) string {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable fixture
			t.Fatal(err)
		}
		return p
	}
	noPath := func(string) (string, error) { return "", errors.New("not in PATH") }

	env := exe("env/icaro")
	if got, err := findIcaro(env, "", "", noPath); got != env || err != nil {
		t.Fatal(got, err)
	}
	if _, err := findIcaro(filepath.Join(dir, "env"), "", "", noPath); err == nil {
		t.Fatal("a directory is not a binary")
	}
	// Bundled next to the app, also through a symlinked app binary.
	app := exe("Icaro.app/Contents/MacOS/icaro-app")
	bundled := exe("Icaro.app/Contents/MacOS/icaro")
	link := filepath.Join(dir, "icaro-app-link")
	if err := os.Symlink(app, link); err != nil {
		t.Fatal(err)
	}
	if got, err := findIcaro("", link, "", noPath); got != bundled || err != nil {
		t.Fatal(got, err)
	}
	// The installer's ~/.local/bin, when nothing is bundled.
	home := filepath.Join(dir, "home")
	local := exe("home/.local/bin/icaro")
	if got, _ := findIcaro("", filepath.Join(dir, "elsewhere", "icaro-app"), home, noPath); got != local && !strings.HasPrefix(got, "/usr/local") && !strings.HasPrefix(got, "/opt/homebrew") {
		t.Fatal(got)
	}
	// The app named icaro is not taken for the CLI.
	self := exe("solo/icaro")
	if got, _ := findIcaro("", self, "", func(string) (string, error) { return "/from/path/icaro", nil }); got == self {
		t.Fatal("the app itself is not the CLI")
	}
	// Not executable.
	plain := filepath.Join(dir, "plain")
	_ = os.WriteFile(plain, nil, 0o600)
	if isExecutable(plain) {
		t.Fatal("mode 0600 is not executable")
	}
	if _, err := FindIcaro(); err != nil && !errors.Is(err, ErrNoIcaro) {
		t.Fatal(err)
	}
}

func TestFindIcaroFallsBackToPath(t *testing.T) {
	if _, err := os.Stat("/usr/local/bin/icaro"); err == nil {
		t.Skip("an installed icaro wins over PATH here")
	}
	if _, err := os.Stat("/opt/homebrew/bin/icaro"); err == nil {
		t.Skip("an installed icaro wins over PATH here")
	}
	got, err := findIcaro("", "", "", func(string) (string, error) { return "/from/path/icaro", nil })
	if got != "/from/path/icaro" || err != nil {
		t.Fatal(got, err)
	}
	if _, err := findIcaro("", "", "", func(string) (string, error) { return "", errors.New("no") }); !errors.Is(err, ErrNoIcaro) {
		t.Fatal(err)
	}
}
