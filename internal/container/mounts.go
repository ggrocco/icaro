package container

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrMountDenied reports a host path the mount policy refuses.
var ErrMountDenied = errors.New("mount denied")

// CredentialDirs are host credential locations, relative to $HOME, that
// are never mounted into a container, nor is any directory that contains
// one (ADR 0011, resolves O4). An explicit shared-home mount masks each of
// them instead.
var CredentialDirs = []string{
	".claude", ".claude.json", ".codex", ".config/gh", ".config/gcloud", ".config/op", ".aws", ".azure", ".ssh",
	".gnupg", ".docker", ".kube", ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".config/git/credentials",
	".icaro", ".local/share/keyrings", "Library/Keychains",
}

// EngineDirs are container engine state directories, relative to $HOME:
// they hold an engine API socket or VM disk (Colima, OrbStack, Rancher
// Desktop, Lima, Podman machine, Docker Desktop). Like credential
// directories they are never mounted (~/.docker is already one).
var EngineDirs = []string{
	".colima", ".orbstack", ".rd", ".lima", ".local/share/containers",
	"Library/Containers/com.docker.docker", "Library/Group Containers/group.com.docker",
}

// credentialFiles are the CredentialDirs entries that are files: a shared
// home masks them with an empty read-only file, not a tmpfs.
var credentialFiles = map[string]bool{
	".claude.json": true, ".netrc": true, ".npmrc": true, ".pypirc": true, ".git-credentials": true, ".config/git/credentials": true,
}

// engineSockets name container engine API sockets; docker*.sock also counts.
var engineSockets = map[string]bool{
	"podman.sock": true, "containerd.sock": true, "buildkitd.sock": true, "crio.sock": true, "dockershim.sock": true, "cri-dockerd.sock": true,
}

// socketScanDepth and socketScanLimit bound the search for an engine
// socket inside a source directory, so a large allowed tree costs little.
// The known engines' deeper sockets live in EngineDirs, refused by path.
const (
	socketScanDepth = 3
	socketScanLimit = 10000
)

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
	// Masks hide credential and engine paths inside a shared home.
	Masks []Mask
}

// Mask hides one path inside a mounted directory.
type Mask struct {
	// Target is the container path.
	Target string
	// File masks a file with an empty read-only file (/dev/null); a
	// directory is covered by an empty tmpfs instead.
	File bool
}

// ParseMount parses "host:container[:ro]". A host path of "~" or one
// starting with "~/" is expanded against the user's home directory, as in
// mounts.allow; the policy check still decides whether it may be mounted.
func ParseMount(spec string) (Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Mount{}, fmt.Errorf("mount %q must be host:container[:ro]", spec)
	}
	m := Mount{Source: parts[0], Target: parts[1]}
	if m.Source == "~" || strings.HasPrefix(m.Source, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Mount{}, fmt.Errorf("mount %q: %w", spec, err)
		}
		m.Source = filepath.Join(home, strings.TrimPrefix(m.Source[1:], "/"))
	}
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
	for _, el := range strings.Split(src, "/") {
		if isEngineSocket(el) {
			return m, fmt.Errorf("%w: the container engine socket is never mounted", ErrMountDenied)
		}
	}
	for _, f := range forbiddenRoots {
		if within(src, f) {
			return m, fmt.Errorf("%w: %s is under %s", ErrMountDenied, src, f)
		}
	}
	home := resolve(p.Home)
	if home != "" {
		protected := protectedPaths(home)
		for _, pp := range protected {
			if within(src, pp.path) {
				return m, fmt.Errorf("%w: %s holds credentials (~/%s) and is never mounted; agents use their profile home", ErrMountDenied, src, pp.rel)
			}
		}
		switch {
		case src == home:
			if !p.SharedHome {
				return m, fmt.Errorf("%w: mounting all of $HOME needs an explicit shared-home profile", ErrMountDenied)
			}
			for _, c := range append(append([]string{}, CredentialDirs...), EngineDirs...) {
				m.Masks = append(m.Masks, Mask{Target: filepath.Join(m.Target, c), File: isFileEntry(home, c)})
			}
			return m, nil
		case within(home, src):
			return m, fmt.Errorf("%w: %s contains $HOME", ErrMountDenied, src)
		}
		// An ancestor of a credential directory (~/.config, ~/.local,
		// ~/Library) would expose it as a subdirectory.
		for _, pp := range protected {
			if within(pp.path, src) {
				return m, fmt.Errorf("%w: %s contains ~/%s, which holds credentials and is never mounted", ErrMountDenied, src, pp.rel)
			}
		}
	}
	allowed := false
	for _, root := range append(append([]string{}, p.Internal...), p.Allow...) {
		if r := resolve(root); r != "" && within(src, r) {
			allowed = true
			break
		}
	}
	if !allowed {
		return m, fmt.Errorf("%w: %s is not under an allowed directory (config mounts.allow)", ErrMountDenied, src)
	}
	if sock := engineSocketIn(src); sock != "" {
		return m, fmt.Errorf("%w: %s holds a container engine socket (%s), which is never mounted", ErrMountDenied, src, sock)
	}
	return m, nil
}

// protectedPath is a credential or engine path under $HOME: as listed
// (rel) and as an absolute host path.
type protectedPath struct{ rel, path string }

// protectedPaths returns every credential and engine path under home,
// both lexically and with symlinks resolved, so ~/.ssh -> ~/dotfiles/ssh
// protects ~/dotfiles/ssh too.
func protectedPaths(home string) []protectedPath {
	var out []protectedPath
	for _, c := range append(append([]string{}, CredentialDirs...), EngineDirs...) {
		lex := filepath.Join(home, c)
		out = append(out, protectedPath{c, lex})
		if r := resolve(lex); r != lex {
			out = append(out, protectedPath{c, r})
		}
	}
	return out
}

// isFileEntry reports whether the protected entry rel under home is a
// file (masked with an empty file) rather than a directory (tmpfs). A
// missing entry is classified by its well-known kind.
func isFileEntry(home, rel string) bool {
	fi, err := os.Stat(filepath.Join(home, rel))
	if err != nil {
		return credentialFiles[rel]
	}
	return !fi.IsDir()
}

// isEngineSocket reports whether name is a container engine socket name.
func isEngineSocket(name string) bool {
	return engineSockets[name] || (strings.HasPrefix(name, "docker") && strings.HasSuffix(name, ".sock"))
}

// engineSocketIn returns the first engine socket at most socketScanDepth
// levels below root, or "" when there is none within socketScanLimit
// entries. Symlinks are not followed: inside the container they resolve
// against the container's filesystem, not the host's.
func engineSocketIn(root string) string {
	found, n := "", 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable here, so unreadable to the container too
		}
		if n++; n > socketScanLimit {
			return filepath.SkipAll
		}
		if d.Type()&fs.ModeSocket != 0 && isEngineSocket(d.Name()) {
			found = p
			return filepath.SkipAll
		}
		if d.IsDir() && strings.Count(strings.TrimPrefix(p, root), "/") >= socketScanDepth {
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// within reports whether p is root or below it.
func within(p, root string) bool {
	if root == "" {
		return false
	}
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// resolve expands "~/", cleans p and resolves symlinks in its longest
// existing prefix, so a path that does not exist (yet) still resolves
// through a symlinked parent.
func resolve(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	p = filepath.Clean(p)
	rest := ""
	for dir := p; ; dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		if dir == filepath.Dir(dir) {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}
