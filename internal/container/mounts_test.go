package container

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestMountPolicy(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "u")
	src := filepath.Join(home, "src", "app")
	other := filepath.Join(root, "other")
	internal := filepath.Join(home, ".icaro", "worktrees")
	for _, d := range []string{src, other, internal, filepath.Join(home, ".ssh"), filepath.Join(home, ".config", "gh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(src, "keys"))
	p := MountPolicy{Allow: []string{filepath.Join(home, "src")}, Internal: []string{internal}, Home: home}

	ok := []string{src, filepath.Join(home, "src")}
	for _, s := range ok {
		if _, err := p.Check(Mount{Source: s, Target: "/x"}); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	denied := []string{
		other,                       // not allow-listed: a file outside the allowlist is not mounted
		filepath.Join(home, ".ssh"), // credentials
		filepath.Join(home, ".config/gh"),
		filepath.Join(src, "keys"),  // symlink into credentials
		home,                        // whole home without shared-home
		filepath.Join(root, "home"), // contains home
		"/var/run/docker.sock", "/proc", "/", "relative/path",
	}
	for _, s := range denied {
		if _, err := p.Check(Mount{Source: s, Target: "/x"}); !errors.Is(err, ErrMountDenied) {
			t.Fatalf("%s: expected denial, got %v", s, err)
		}
	}
	// .icaro is a credential dir as a whole, but its managed subtrees are
	// mounted through Internal... which lives under ~/.icaro: Ícaro mounts
	// those itself, never through user policy.
	if _, err := p.Check(Mount{Source: internal, Target: "/w"}); !errors.Is(err, ErrMountDenied) {
		t.Fatal("user mounts of ~/.icaro must be denied")
	}

	p.SharedHome = true
	m, err := p.Check(Mount{Source: home, Target: "/home/host"})
	if err != nil || len(m.Masks) != len(CredentialDirs)+len(EngineDirs) || m.Masks[0] != (Mask{Target: "/home/host/.claude"}) {
		t.Fatalf("shared home: %+v %v", m, err)
	}
}

// TestMountPolicyCredentialAncestors: a directory that contains a
// credential or engine directory is refused even when it is allowed, so
// mounts.allow ["~"] cannot expose ~/.config/gh through ~/.config.
func TestMountPolicyCredentialAncestors(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dotfiles := filepath.Join(root, "dotfiles")
	for _, d := range []string{
		filepath.Join(home, "src", "app"), filepath.Join(home, ".config", "gh"), filepath.Join(home, ".config", "nvim"),
		filepath.Join(home, ".local", "share", "keyrings"), filepath.Join(home, "Library", "Keychains"),
		filepath.Join(home, ".colima", "default"), filepath.Join(dotfiles, "ssh"), filepath.Join(dotfiles, "vim"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// ~/.ssh is a symlink into a dotfiles checkout outside $HOME.
	if err := os.Symlink(filepath.Join(dotfiles, "ssh"), filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	p := MountPolicy{Allow: []string{home, dotfiles}, Home: home}
	if _, err := p.Check(Mount{Source: filepath.Join(home, "src"), Target: "/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Check(Mount{Source: filepath.Join(home, ".config", "nvim"), Target: "/x"}); err != nil {
		t.Fatalf("a sibling of a credential directory is fine: %v", err)
	}
	for _, s := range []string{
		filepath.Join(home, ".config"), filepath.Join(home, ".local"), filepath.Join(home, ".local", "share"),
		filepath.Join(home, "Library"), filepath.Join(home, ".colima"), filepath.Join(home, ".colima", "default"),
		filepath.Join(dotfiles, "ssh"), // the resolved target of ~/.ssh
		dotfiles,                       // contains it
	} {
		if _, err := p.Check(Mount{Source: s, Target: "/x"}); !errors.Is(err, ErrMountDenied) {
			t.Fatalf("%s: expected denial, got %v", s, err)
		}
	}
}

// TestMountPolicyEngineSockets: a source that is, or holds near its top,
// a container engine socket is refused.
func TestMountPolicyEngineSockets(t *testing.T) {
	root, err := os.MkdirTemp("", "icm") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, _ = filepath.EvalSymlinks(root)
	listen := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	listen(filepath.Join(root, "vm", "run", "docker.sock"))
	listen(filepath.Join(root, "pod", "podman.sock"))
	listen(filepath.Join(root, "app", "pg.sock"))                     // not an engine socket
	listen(filepath.Join(root, "deep", "a", "b", "c", "docker.sock")) // below the scan depth
	if err := os.WriteFile(filepath.Join(root, "app", "docker.sock.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := MountPolicy{Allow: []string{root}}
	for _, s := range []string{filepath.Join(root, "vm"), filepath.Join(root, "vm", "run"), filepath.Join(root, "pod"), filepath.Join(root, "vm", "run", "docker.sock")} {
		if _, err := p.Check(Mount{Source: s, Target: "/x"}); !errors.Is(err, ErrMountDenied) {
			t.Fatalf("%s: expected denial, got %v", s, err)
		}
	}
	for _, s := range []string{filepath.Join(root, "app"), filepath.Join(root, "deep")} {
		if _, err := p.Check(Mount{Source: s, Target: "/x"}); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if !isEngineSocket("docker.raw.sock") || isEngineSocket("pg.sock") || isEngineSocket("docker") {
		t.Fatal("engine socket names")
	}
}

// TestSharedHomeMasks: a shared home masks directories with a tmpfs and
// files with an empty read-only file, present or not.
func TestSharedHomeMasks(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".npmrc"), 0o700); err != nil { // odd, but a directory
		t.Fatal(err)
	}
	m, err := MountPolicy{Home: home, SharedHome: true}.Check(Mount{Source: home, Target: "/h"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, mk := range m.Masks {
		got[mk.Target] = mk.File
	}
	want := map[string]bool{"/h/.ssh": false, "/h/.claude.json": true, "/h/.netrc": true, "/h/.aws": false, "/h/.npmrc": false, "/h/.colima": false}
	for target, file := range want {
		if f, ok := got[target]; !ok || f != file {
			t.Fatalf("%s: masked=%v file=%v, want file=%v", target, ok, f, file)
		}
	}
}

func TestParseMount(t *testing.T) {
	m, err := ParseMount("/a:/b:ro")
	if err != nil || !m.ReadOnly || m.Source != "/a" || m.Target != "/b" {
		t.Fatal(m, err)
	}
	for _, bad := range []string{"/a", "/a:b", "/a:/b:x", ":/b", "/a:/b:ro:x"} {
		if _, err := ParseMount(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// TestParseMountExpandsHome: the docs/sessions.md example
// "~/datasets/fixtures:/data:ro" expands against $HOME and passes the
// policy when ~/datasets is allowed.
func TestParseMountExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixtures := filepath.Join(home, "datasets", "fixtures")
	if err := os.MkdirAll(fixtures, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := ParseMount("~/datasets/fixtures:/data:ro")
	if err != nil || m.Source != fixtures || m.Target != "/data" || !m.ReadOnly {
		t.Fatalf("%+v %v", m, err)
	}
	p := MountPolicy{Allow: []string{"~/datasets"}, Home: home}
	if _, err := p.Check(m); err != nil {
		t.Fatalf("docs example refused: %v", err)
	}
	if m, err := ParseMount("~:/h"); err != nil || m.Source != home {
		t.Fatalf("~ alone: %+v %v", m, err)
	}
	// "~user/..." is not expanded: it stays relative and the policy refuses it.
	m, err = ParseMount("~other/x:/x")
	if err != nil || m.Source != "~other/x" {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := p.Check(m); !errors.Is(err, ErrMountDenied) {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	if _, err := ParseMount("~/x:/x"); err == nil {
		t.Fatal("an unknown home must fail the mount")
	}
}

func TestMountPolicyResolution(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "src")
	if err := os.MkdirAll(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	// A missing source cannot be resolved, so it is denied.
	p := MountPolicy{Allow: []string{allowed}}
	if _, err := p.Check(Mount{Source: filepath.Join(allowed, "missing"), Target: "/x"}); !errors.Is(err, ErrMountDenied) {
		t.Fatal(err)
	}
	// An allow entry that does not exist (yet) still matches by its
	// cleaned path; "~/" entries expand to the user's home.
	p = MountPolicy{Allow: []string{filepath.Join(root, "later") + "/", "~/nonexistent-icaro-test", allowed}}
	if _, err := p.Check(Mount{Source: allowed, Target: "/x"}); err != nil {
		t.Fatal(err)
	}
	if h, err := os.UserHomeDir(); err == nil && resolve("~/nonexistent-icaro-test") != filepath.Join(h, "nonexistent-icaro-test") {
		t.Fatalf("~/ must expand to the home directory: %s", resolve("~/nonexistent-icaro-test"))
	}
	// A missing path resolves through its existing, symlinked parent.
	link := filepath.Join(root, "link")
	if err := os.Symlink(allowed, link); err != nil {
		t.Fatal(err)
	}
	if got := resolve(filepath.Join(link, "a", "b")); got != filepath.Join(resolve(allowed), "a", "b") {
		t.Fatalf("resolve through a symlinked parent: %s", got)
	}
	if resolve("relative") != "relative" {
		t.Fatal(resolve("relative"))
	}
	if resolve("") != "" || within("/a", "") {
		t.Fatal("an empty root matches nothing")
	}
	if !within("/a/b", "/a/") || within("/ab", "/a") {
		t.Fatal("within must compare whole path elements")
	}
}
