// Package sandbox builds the hardened container host configuration for
// steps and agent sessions. Nothing in the workflow schema can weaken
// these defaults; a profile can only add a stronger OCI runtime (gVisor's
// runsc) underneath them.
package sandbox

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

//go:embed seccomp.json
var seccompProfile string

// Networks.
const (
	NetworkEgress = "egress"
	NetworkNone   = "none"
)

// Mount targets inside every step container.
const (
	WorkspacePath = "/workspace"
	IOPath        = "/icaro"
)

// Limits are the effective resource ceilings for a container.
type Limits struct {
	MemoryBytes int64
	NanoCPUs    int64
	Pids        int64
	NoFile      int64
}

// Options select how a step container is confined.
type Options struct {
	Network         string // egress | none
	Limits          Limits
	WorkspaceVolume string
	IOVolume        string
	// Runtime is the OCI runtime (e.g. runsc); empty is Docker's default.
	Runtime string
}

// Tighten returns the stricter of the configured default and a requested
// value; zero requests keep the default. Requests may only lower limits.
func Tighten(def, requested int64) int64 {
	if requested <= 0 || requested > def {
		return def
	}
	return requested
}

// HostConfig builds the container host configuration for the options.
func HostConfig(o Options) (*container.HostConfig, error) {
	if o.Network == "" {
		o.Network = NetworkEgress
	}
	if o.WorkspaceVolume == "" || o.IOVolume == "" {
		return nil, fmt.Errorf("sandbox: workspace and io volumes are required")
	}
	pids := o.Limits.Pids
	if pids <= 0 {
		pids = 256
	}
	nofile := o.Limits.NoFile
	if nofile <= 0 {
		nofile = 4096
	}
	netMode := container.NetworkMode("bridge")
	if o.Network == NetworkNone {
		netMode = container.NetworkMode("none")
	}

	hc := &container.HostConfig{
		NetworkMode:    netMode,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges", "seccomp=" + strings.TrimSpace(seccompProfile)},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp":  "rw,nosuid,nodev,noexec,size=256m",
			"/home": "rw,nosuid,nodev,size=64m",
			"/run":  "rw,nosuid,nodev,noexec,size=16m",
		},
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: o.WorkspaceVolume, Target: WorkspacePath},
			{Type: mount.TypeVolume, Source: o.IOVolume, Target: IOPath},
		},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "50m", "max-file": "2"}},
	}
	hc.Resources = container.Resources{
		Memory:     o.Limits.MemoryBytes,
		MemorySwap: o.Limits.MemoryBytes, // no swap beyond the memory limit
		NanoCPUs:   o.Limits.NanoCPUs,
		PidsLimit:  &pids,
		Ulimits: []*container.Ulimit{
			{Name: "nofile", Soft: nofile, Hard: nofile},
			{Name: "core", Soft: 0, Hard: 0},
		},
	}

	hc.Runtime = o.Runtime
	return hc, nil
}

// ForbiddenHostPaths are never mountable, even if a future schema field
// allowed mounts; validation rejects them up front.
var ForbiddenHostPaths = []string{"/var/run", "/run", "/proc", "/sys", "/dev", "/etc", "/"}

// IsForbiddenPath reports whether p is (under) a forbidden host path.
func IsForbiddenPath(p string) bool {
	for _, f := range ForbiddenHostPaths {
		if p == f || (f != "/" && strings.HasPrefix(p, f+"/")) {
			return true
		}
	}
	return false
}
