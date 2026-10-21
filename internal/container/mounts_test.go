package container

import (
	"errors"
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
	if err != nil || len(m.Masks) != len(CredentialDirs) || m.Masks[0] != "/home/host/.claude" {
		t.Fatalf("shared home: %+v %v", m, err)
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
	if resolve("") != "" || within("/a", "") {
		t.Fatal("an empty root matches nothing")
	}
	if !within("/a/b", "/a/") || within("/ab", "/a") {
		t.Fatal("within must compare whole path elements")
	}
}
