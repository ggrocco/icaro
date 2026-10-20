// Package creds manages the per-profile persistent homes (ADR 0005).
//
// A profile home is a golden seed layer under ~/.icaro/home/<profile>/seed
// that holds the agent's login, plus one writable layer per session for
// runtime writes. Sessions sharing a profile share the login and never
// write into each other's (or the seed's) files. The host's real ~/.claude
// is never involved.
//
// On Linux the layers are combined by an overlay mount that the Docker
// daemon performs (local volume driver, type=overlay). Where that is not
// possible (Docker Desktop's VM cannot overlay host paths) the session
// layer is a full copy of the seed — spike S2's fallback.
package creds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"icaro/internal/container"
	"icaro/internal/paths"
)

// Modes.
const (
	ModeAuto    = "auto"
	ModeOverlay = "overlay"
	ModeCopy    = "copy"
)

// Volumes is the Docker surface for overlay homes.
type Volumes interface {
	CreateVolumeOpts(ctx context.Context, name, driver string, opts, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
}

// Homes manages profile homes.
type Homes struct {
	Layout  paths.Layout
	Mode    string
	Volumes Volumes

	overlayBroken atomic.Bool
}

// LabelHome marks home volumes.
const LabelHome = "icaro.home"

// SeedDir is the golden layer of profile.
func (h *Homes) SeedDir(profile string) string { return filepath.Join(h.Layout.Home(profile), "seed") }

func (h *Homes) sessionDir(profile, sessionID string) string {
	return filepath.Join(h.Layout.Home(profile), "sessions", sessionID)
}

// mode returns the effective mode.
func (h *Homes) mode() string {
	switch h.Mode {
	case ModeOverlay, ModeCopy:
		return h.Mode
	}
	if runtime.GOOS == "linux" && h.Volumes != nil && !h.overlayBroken.Load() {
		return ModeOverlay
	}
	return ModeCopy
}

// DisableOverlay switches auto mode to copies, e.g. after the Docker
// daemon refused an overlay mount.
func (h *Homes) DisableOverlay() { h.overlayBroken.Store(true) }

// OverlayEnabled reports whether new sessions get overlay homes.
func (h *Homes) OverlayEnabled() bool { return h.mode() == ModeOverlay }

// VolumeName is the overlay volume of a session.
func VolumeName(sessionID string) string { return "icaro-home-" + strings.ToLower(sessionID) }

// EnsureSeed creates the seed directory of profile.
func (h *Homes) EnsureSeed(profile string) (string, error) {
	seed := h.SeedDir(profile)
	if err := os.MkdirAll(seed, 0o700); err != nil {
		return "", err
	}
	return seed, nil
}

// Prepare returns the home mount for a session, creating its writable
// layer. Calling it again for the same session reuses the layer, so a
// stopped session keeps its runtime state.
func (h *Homes) Prepare(ctx context.Context, profile, sessionID string) (container.HomeMount, error) {
	seed, err := h.EnsureSeed(profile)
	if err != nil {
		return container.HomeMount{}, err
	}
	dir := h.sessionDir(profile, sessionID)
	if h.mode() == ModeOverlay {
		upper, work := filepath.Join(dir, "upper"), filepath.Join(dir, "work")
		for _, d := range []string{upper, work} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return container.HomeMount{}, err
			}
		}
		name := VolumeName(sessionID)
		opts := map[string]string{
			"type":   "overlay",
			"device": "overlay",
			"o":      fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", seed, upper, work),
		}
		if err := h.Volumes.CreateVolumeOpts(ctx, name, "local", opts, map[string]string{LabelHome: profile}); err != nil {
			return container.HomeMount{}, fmt.Errorf("home overlay volume: %w", err)
		}
		return container.HomeMount{Volume: name}, nil
	}
	home := filepath.Join(dir, "home")
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		if err := copyTree(seed, home); err != nil {
			return container.HomeMount{}, fmt.Errorf("copy profile home: %w", err)
		}
	}
	return container.HomeMount{Dir: home}, nil
}

// writable is the session's writable layer on the host.
func (h *Homes) writable(profile, sessionID string) string {
	dir := h.sessionDir(profile, sessionID)
	if _, err := os.Stat(filepath.Join(dir, "upper")); err == nil {
		return filepath.Join(dir, "upper")
	}
	return filepath.Join(dir, "home")
}

// Read returns a home file as the session sees it (its layer, then the seed).
func (h *Homes) Read(profile, sessionID, rel string) ([]byte, bool, error) {
	for _, base := range []string{h.writable(profile, sessionID), h.SeedDir(profile)} {
		b, err := os.ReadFile(filepath.Join(base, rel))
		if err == nil {
			return b, true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
	}
	return nil, false, nil
}

// Write writes a file into the session's layer only; the seed is never
// touched by a session. Call it after Prepare.
func (h *Homes) Write(profile, sessionID, rel string, data []byte) error {
	if strings.Contains(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("invalid home path %q", rel)
	}
	p := filepath.Join(h.writable(profile, sessionID), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// Release removes a session's layer (and overlay volume).
func (h *Homes) Release(ctx context.Context, profile, sessionID string) error {
	if h.Volumes != nil {
		if err := h.Volumes.RemoveVolume(ctx, VolumeName(sessionID)); err != nil {
			return err
		}
	}
	return os.RemoveAll(h.sessionDir(profile, sessionID))
}

// copyTree copies src to dst preserving modes; symlinks are copied as links.
// Both trees are daemon-owned under ~/.icaro (0700) and no container runs
// on them while they are copied, so there is no racing writer.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p) //nolint:gosec // see copyTree
			if err != nil {
				return err
			}
			return os.Symlink(link, target) //nolint:gosec // see copyTree
		case info.Mode().IsRegular():
			in, err := os.Open(p) //nolint:gosec // see copyTree: daemon-owned tree, no concurrent writer
			if err != nil {
				return err
			}
			defer func() { _ = in.Close() }()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()) //nolint:gosec // see copyTree
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				_ = out.Close()
				return err
			}
			return out.Close()
		}
		return nil // sockets, devices: skip
	})
}
