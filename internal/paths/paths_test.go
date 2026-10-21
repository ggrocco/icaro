package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	t.Setenv(RootEnv, "/custom/root")
	if got := Default().Root; got != "/custom/root" {
		t.Fatalf("env root = %q", got)
	}

	t.Setenv(RootEnv, "")
	t.Setenv("HOME", "/home/someone")
	if got := Default().Root; got != filepath.Join("/home/someone", ".icaro") {
		t.Fatalf("home root = %q", got)
	}

	// Without a resolvable home the layout falls back to a relative root.
	t.Setenv("HOME", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("home directory still resolvable on this platform")
	}
	if got := Default().Root; got != ".icaro" {
		t.Fatalf("fallback root = %q", got)
	}
}

func TestLayout(t *testing.T) {
	l := New("/r")
	for got, want := range map[string]string{
		l.RunDir():        "/r/run",
		l.Lock():          "/r/run/icaro.lock",
		l.PID():           "/r/run/icaro.pid",
		l.Socket():        "/r/run/icaro.sock",
		l.ConfigDir():     "/r/config",
		l.ConfigFile():    "/r/config/icaro.yaml",
		l.DB():            "/r/config/icaro.db",
		l.Logs():          "/r/logs",
		l.DaemonLog():     "/r/logs/daemon.log",
		l.Homes():         "/r/home",
		l.Home("default"): "/r/home/default",
		l.Worktrees():     "/r/worktrees",
		l.Repos():         "/r/repos",
		l.Runs():          "/r/runs",
		l.Integrations():  "/r/integrations",
		l.Secrets():       "/r/secrets",
		l.Previews():      "/r/previews",
		l.Skills():        "/r/skills",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestEnsureCreatesPrivateDirs(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "root"))
	// A pre-existing, too-open directory is tightened.
	if err := os.MkdirAll(l.Logs(), 0o755); err != nil { //nolint:gosec // deliberately too open

		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range l.PrivateDirs() {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v", d, fi.Mode())
		}
	}
}

func TestEnsureFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	// A file where a directory should be makes MkdirAll fail.
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New(root).Ensure(); err == nil {
		t.Fatal("Ensure over a file must fail")
	}
}
