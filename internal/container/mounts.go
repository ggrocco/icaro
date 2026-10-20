package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrMountDenied reports a host path the mount policy refuses.
var ErrMountDenied = errors.New("mount denied")

// CredentialDirs are host credential locations, relative to $HOME, that
// are never mounted into a container (ADR 0011, resolves O4). An explicit
// shared-home mount gets an empty tmpfs over each of them instead.
var CredentialDirs = []string{
	".claude", ".claude.json", ".codex", ".config/gh", ".config/gcloud", ".config/op", ".aws", ".azure", ".ssh",
	".gnupg", ".docker", ".kube", ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".config/git/credentials",
	".icaro", ".local/share/keyrings", "Library/Keychains",
}

// forbiddenRoots are never mountable, nor is anything below them.
var forbiddenRoots = []string{"/var/run", "/run", "/proc", "/sys", "/dev", "/etc", "/boot", "/private/var/run", "/private/etc"}

// MountPolicy decides which host paths may be bind-mounted.
type MountPolicy struct {
	// Allow lists host directories (and their subtrees) that may be mounted.
	Allow []string
	// Internal lists Ícaro-managed roots (worktrees, homes, runs) that are
	// always mountable.
	Internal []string
	// Home is the user's home directory.
	Home string
	// SharedHome permits mounting Home itself (an explicit profile choice);
	// credential directories are masked, never exposed.
	SharedHome bool
}

// Mount is a checked bind mount.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
	// Masks are container paths covered by an empty tmpfs (credential dirs
	// inside a shared home).
	Masks []string
}

// ParseMount parses "host:container[:ro]".
func ParseMount(spec string) (Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Mount{}, fmt.Errorf("mount %q must be host:container[:ro]", spec)
	}
	m := Mount{Source: parts[0], Target: parts[1]}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			m.ReadOnly = true
		case "rw":
		default:
			return Mount{}, fmt.Errorf("mount %q: mode must be ro or rw", spec)
		}
	}
	if !filepath.IsAbs(m.Target) {
		return Mount{}, fmt.Errorf("mount %q: container path must be absolute", spec)
	}
	return m, nil
}

// Check validates a mount, resolving symlinks so a link cannot smuggle a
// forbidden path in. It returns the mount with its resolved source.
func (p MountPolicy) Check(m Mount) (Mount, error) {
	if !filepath.IsAbs(m.Source) {
		return m, fmt.Errorf("%w: %s is not an absolute path", ErrMountDenied, m.Source)
	}
	src, err := filepath.EvalSymlinks(filepath.Clean(m.Source))
	if err != nil {
		return m, fmt.Errorf("%w: %s: %v", ErrMountDenied, m.Source, err)
	}
	m.Source = src
	if src == "/" {
		return m, fmt.Errorf("%w: the host root cannot be mounted", ErrMountDenied)
	}
	if strings.HasSuffix(src, "docker.sock") || strings.Contains(src, "/docker.sock") {
		return m, fmt.Errorf("%w: the Docker socket is never mounted", ErrMountDenied)
	}
	for _, f := range forbiddenRoots {
		if within(src, f) {
			return m, fmt.Errorf("%w: %s is under %s", ErrMountDenied, src, f)
		}
	}
	home := resolve(p.Home)
	if home != "" {
		for _, c := range CredentialDirs {
			cred := filepath.Join(home, c)
			if within(src, cred) {
				return m, fmt.Errorf("%w: %s holds credentials (~/%s) and is never mounted; agents use their profile home", ErrMountDenied, src, c)
			}
		}
		switch {
		case src == home:
			if !p.SharedHome {
				return m, fmt.Errorf("%w: mounting all of $HOME needs an explicit shared-home profile", ErrMountDenied)
			}
			for _, c := range CredentialDirs {
				m.Masks = append(m.Masks, filepath.Join(m.Target, c))
			}
			return m, nil
		case within(home, src):
			return m, fmt.Errorf("%w: %s contains $HOME", ErrMountDenied, src)
		}
	}
	for _, root := range append(append([]string{}, p.Internal...), p.Allow...) {
		if r := resolve(root); r != "" && within(src, r) {
			return m, nil
		}
	}
	return m, fmt.Errorf("%w: %s is not under an allowed directory (config mounts.allow)", ErrMountDenied, src)
}

// within reports whether p is root or below it.
func within(p, root string) bool {
	if root == "" {
		return false
	}
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

func resolve(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if r, err := filepath.EvalSymlinks(filepath.Clean(p)); err == nil {
		return r
	}
	return filepath.Clean(p)
}
