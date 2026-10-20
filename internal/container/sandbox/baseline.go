package sandbox

import (
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// BaselineOptions describe a container that is not a workflow step: agent
// session containers and Ícaro's helper containers. The hardening is the
// same container baseline as HostConfig (03-security); only the mounts,
// network and home differ.
type BaselineOptions struct {
	Limits Limits
	Mounts []mount.Mount
	// NetworkMode is the network to join; empty means "none".
	NetworkMode string
	// TmpfsHome mounts a tmpfs at /home (containers without a profile home).
	TmpfsHome bool
	// Runtime is an optional OCI runtime (e.g. runsc).
	Runtime string
}

// Baseline returns the hardened host configuration for o.
func Baseline(o BaselineOptions) *container.HostConfig {
	pids := o.Limits.Pids
	if pids <= 0 {
		pids = 512
	}
	nofile := o.Limits.NoFile
	if nofile <= 0 {
		nofile = 4096
	}
	netMode := o.NetworkMode
	if netMode == "" {
		netMode = "none"
	}
	tmpfs := map[string]string{
		"/tmp": "rw,nosuid,nodev,size=512m",
		"/run": "rw,nosuid,nodev,noexec,size=16m",
	}
	if o.TmpfsHome {
		tmpfs["/home"] = "rw,nosuid,nodev,size=64m"
	}
	hc := &container.HostConfig{
		NetworkMode:    container.NetworkMode(netMode),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges", "seccomp=" + strings.TrimSpace(seccompProfile)},
		ReadonlyRootfs: true,
		Tmpfs:          tmpfs,
		Mounts:         o.Mounts,
		Init:           boolPtr(true),
		LogConfig:      container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "20m", "max-file": "2"}},
		Runtime:        o.Runtime,
	}
	hc.Resources = container.Resources{
		Memory:     o.Limits.MemoryBytes,
		MemorySwap: o.Limits.MemoryBytes,
		NanoCPUs:   o.Limits.NanoCPUs,
		PidsLimit:  &pids,
		Ulimits: []*container.Ulimit{
			{Name: "nofile", Soft: nofile, Hard: nofile},
			{Name: "core", Soft: 0, Hard: 0},
		},
	}
	return hc
}

// SeccompProfile returns the embedded seccomp profile (for the docker CLI,
// which takes it as a file).
func SeccompProfile() string { return seccompProfile }

func boolPtr(b bool) *bool { return &b }
