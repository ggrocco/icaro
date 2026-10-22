package creds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"icaro/internal/paths"
)

// fakeFiles is a HomeFiles with per-path contents and injected failures.
type fakeFiles struct {
	files    map[string]string
	readErr  map[string]error
	writeErr map[string]error
}

func (f *fakeFiles) Read(rel string) ([]byte, bool, error) {
	if err := f.readErr[rel]; err != nil {
		return nil, false, err
	}
	v, ok := f.files[rel]
	return []byte(v), ok, nil
}

func (f *fakeFiles) Write(rel string, data []byte) error {
	if err := f.writeErr[rel]; err != nil {
		return err
	}
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[rel] = string(data)
	return nil
}

var errIO = errors.New("io failure")

// TestConfigureErrors: a failing read or write of any agent file aborts
// configuration with that error, and a corrupt .claude.json is reported
// instead of being overwritten.
func TestConfigureErrors(t *testing.T) {
	claude, codex := Agents["claude"], Agents["codex"]
	for _, tc := range []struct {
		name  string
		agent Agent
		files *fakeFiles
	}{
		{"claude config read", claude, &fakeFiles{readErr: map[string]error{".claude.json": errIO}}},
		{"claude config write", claude, &fakeFiles{writeErr: map[string]error{".claude.json": errIO}}},
		{"claude md read", claude, &fakeFiles{readErr: map[string]error{".claude/CLAUDE.md": errIO}}},
		{"codex config read", codex, &fakeFiles{readErr: map[string]error{".codex/config.toml": errIO}}},
		{"codex config write", codex, &fakeFiles{writeErr: map[string]error{".codex/config.toml": errIO}}},
		{"codex md read", codex, &fakeFiles{readErr: map[string]error{".codex/AGENTS.md": errIO}}},
	} {
		if err := tc.agent.Configure(tc.files, true, "p"); !errors.Is(err, errIO) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	corrupt := &fakeFiles{files: map[string]string{".claude.json": "{not json"}}
	if err := claude.Configure(corrupt, true, "p"); err == nil || !strings.Contains(err.Error(), ".claude.json") {
		t.Fatalf("corrupt .claude.json: %v", err)
	}
	if corrupt.files[".claude.json"] != "{not json" {
		t.Fatal("corrupt user config overwritten")
	}
}

func TestConfigureWithoutMCP(t *testing.T) {
	// Without memory the agent configs are left alone; only the preamble is written.
	for _, name := range []string{"claude", "codex"} {
		f := &fakeFiles{}
		if err := Agents[name].Configure(f, false, "p"); err != nil {
			t.Fatal(err)
		}
		if len(f.files) != 1 {
			t.Fatalf("%s wrote %v", name, f.files)
		}
	}
}

func TestClaudeConfigWithoutServers(t *testing.T) {
	f := &fakeFiles{files: map[string]string{".claude.json": `{"theme":"dark"}`}}
	if err := Agents["claude"].Configure(f, true, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.files[".claude.json"], `"theme": "dark"`) || !strings.Contains(f.files[".claude.json"], `"icaro"`) {
		t.Fatal(f.files[".claude.json"])
	}
}

func TestCodexConfigAppendsAfterUnterminatedLine(t *testing.T) {
	f := &fakeFiles{files: map[string]string{".codex/config.toml": `model = "o3"`}}
	if err := Agents["codex"].Configure(f, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.files[".codex/config.toml"]; !strings.HasPrefix(got, "model = \"o3\"\n\n[mcp_servers.icaro]") {
		t.Fatalf("%q", got)
	}
}

func TestAutoModePicksOverlayOnLinux(t *testing.T) {
	h := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeAuto, Volumes: &fakeVolumes{}}
	if got, want := h.OverlayEnabled(), runtime.GOOS == "linux"; got != want {
		t.Fatalf("overlay = %v on %s", got, runtime.GOOS)
	}
	if (&Homes{Mode: ModeAuto}).OverlayEnabled() {
		t.Fatal("overlay needs a Docker volume driver")
	}
}

type failingVolumes struct{ create, remove error }

func (f failingVolumes) CreateVolumeOpts(context.Context, string, string, map[string]string, map[string]string) error {
	return f.create
}
func (f failingVolumes) RemoveVolume(context.Context, string) error { return f.remove }

func TestHomeErrors(t *testing.T) {
	ctx := context.Background()

	// The home root is a file: no seed, no session layer.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "home"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Homes{Layout: paths.New(root), Mode: ModeCopy}
	if _, err := h.EnsureSeed("claude"); err == nil {
		t.Fatal("EnsureSeed over a file")
	}
	if _, err := h.Prepare(ctx, "claude", "S"); err == nil {
		t.Fatal("Prepare over a file")
	}

	// The sessions directory is a file: overlay layers cannot be created.
	ov := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeOverlay, Volumes: &fakeVolumes{}}
	if _, err := ov.EnsureSeed("codex"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ov.Layout.Home("codex"), "sessions"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.Prepare(ctx, "codex", "S"); err == nil {
		t.Fatal("overlay layers over a file")
	}
	// ...and the copy fallback must not hand out a home it could not check
	// (regression: a non-ENOENT stat error used to return a missing dir).
	cp := &Homes{Layout: ov.Layout, Mode: ModeCopy}
	if m, err := cp.Prepare(ctx, "codex", "S"); err == nil {
		t.Fatalf("copy over a file returned %+v", m)
	}
	// The session directory is a dangling symlink: copying the seed fails.
	cp2 := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeCopy}
	if _, err := cp2.EnsureSeed("codex"); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(cp2.Layout.Home("codex"), "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sessions, "nowhere"), filepath.Join(sessions, "S")); err != nil {
		t.Fatal(err)
	}
	if _, err := cp2.Prepare(ctx, "codex", "S"); err == nil || !strings.Contains(err.Error(), "copy profile home") {
		t.Fatalf("copy into a dangling link: %v", err)
	}

	// Docker refuses the overlay volume, or its removal.
	broken := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeOverlay, Volumes: failingVolumes{create: errIO, remove: errIO}}
	if _, err := broken.Prepare(ctx, "codex", "S"); !errors.Is(err, errIO) {
		t.Fatalf("volume create: %v", err)
	}
	if err := broken.Release(ctx, "codex", "S"); !errors.Is(err, errIO) {
		t.Fatalf("volume remove: %v", err)
	}

	// Reading through a file used as a directory is an error, not "absent".
	rw := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeCopy}
	if _, err := rw.Prepare(ctx, "claude", "S"); err != nil {
		t.Fatal(err)
	}
	if err := rw.Write("claude", "S", ".claude", []byte("file")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rw.Read("claude", "S", ".claude/settings.json"); err == nil {
		t.Fatal("read through a file")
	}
	if err := rw.Write("claude", "S", ".claude/settings.json", nil); err == nil {
		t.Fatal("write through a file")
	}
	if err := rw.Write("claude", "S", "/etc/passwd", nil); err == nil {
		t.Fatal("absolute path accepted")
	}
	if _, ok, err := rw.Read("claude", "S", "absent"); ok || err != nil {
		t.Fatalf("absent file: %v %v", ok, err)
	}
}

// TestCopyTree: symlinks stay links, special files are skipped, and copy
// errors are reported.
func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("data"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "f")); string(got) != "data" {
		t.Fatalf("file: %q", got)
	}
	if fi, err := os.Stat(filepath.Join(dst, "f")); err != nil || fi.Mode().Perm() != 0o400 {
		t.Fatalf("mode not preserved: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "f" {
		t.Fatalf("symlink: %q %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "fifo")); !os.IsNotExist(err) {
		t.Fatal("special file copied")
	}

	if err := copyTree(filepath.Join(src, "missing"), t.TempDir()); err == nil {
		t.Fatal("missing source")
	}
	// The destination already has a directory where a file goes.
	clash := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clash, "f"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, clash); err == nil {
		t.Fatal("file over a directory")
	}
	// An unreadable seed file fails the copy (root reads it regardless).
	if os.Geteuid() != 0 {
		locked := t.TempDir()
		if err := os.WriteFile(filepath.Join(locked, "secret"), nil, 0o000); err != nil {
			t.Fatal(err)
		}
		if err := copyTree(locked, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Fatal("unreadable file copied")
		}
	}
	// ...or a file where a link goes.
	clash2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(clash2, "link"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, clash2); err == nil {
		t.Fatal("link over a file")
	}
}
