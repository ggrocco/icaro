package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"icaro/internal/container/docker"
)

// ProxyName is the Ícaro-managed Traefik container.
const ProxyName = "icaro-traefik"

// proxyDynamicDir is where Traefik reads its file-provider config.
const proxyDynamicDir = "/etc/icaro/dynamic"

// Engine is the Docker surface the preview manager needs.
type Engine interface {
	EnsureImage(ctx context.Context, ref string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) (bool, error)
	NetworkExists(ctx context.Context, name string) (bool, error)
	CreateProxy(ctx context.Context, s docker.ProxySpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	Inspect(ctx context.Context, id string) (docker.State, error)
	RemoveContainer(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
}

// ProxyOptions configure the Traefik container.
type ProxyOptions struct {
	Image      string
	HTTPPort   int
	DynamicDir string // host directory holding one file per preview
	MacOS      bool
}

// proxyConfigHash identifies the container's configuration; a mismatch
// (image, port, dir) recreates it.
func (o ProxyOptions) hash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|v1", o.Image, o.HTTPPort, o.DynamicDir)))
	return hex.EncodeToString(sum[:])[:12]
}

// EnsureProxy makes sure the preview network exists and Traefik runs on
// it with the current configuration. It is idempotent.
//
// Traefik uses its file provider: Ícaro writes routes into DynamicDir, so
// the proxy needs no Docker socket (ADR 0017).
func EnsureProxy(ctx context.Context, e Engine, o ProxyOptions) error {
	if _, err := e.EnsureNetwork(ctx, Network, map[string]string{"icaro.managed": "preview"}); err != nil {
		return err
	}
	if err := os.MkdirAll(o.DynamicDir, 0o700); err != nil {
		return err
	}
	st, err := e.Inspect(ctx, ProxyName)
	if err != nil {
		return err
	}
	want := o.hash()
	if st.Exists && st.Labels["icaro.proxy.config"] == want {
		if !st.Running {
			return e.StartContainer(ctx, ProxyName)
		}
		return nil
	}
	if st.Exists {
		if err := e.RemoveContainer(ctx, ProxyName); err != nil {
			return err
		}
	}
	if err := e.EnsureImage(ctx, o.Image); err != nil {
		return err
	}
	pids := int64(128)
	hc := &container.HostConfig{
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=16m"},
		Mounts: []mount.Mount{{
			Type: mount.TypeBind, Source: o.DynamicDir, Target: proxyDynamicDir, ReadOnly: true,
		}},
		Resources: container.Resources{Memory: 256 << 20, PidsLimit: &pids},
	}
	id, err := e.CreateProxy(ctx, docker.ProxySpec{
		Name:  ProxyName,
		Image: o.Image,
		Cmd: []string{
			"--entrypoints.web.address=:8080",
			"--providers.file.directory=" + proxyDynamicDir,
			"--providers.file.watch=true",
			"--ping=true",
			"--ping.entrypoint=web",
			"--log.level=ERROR",
			"--global.checknewversion=false",
			"--global.sendanonymoususage=false",
		},
		// Run as the daemon's user so it can read the 0700 dynamic dir and
		// needs no capability to bind its unprivileged port.
		User:          strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		Labels:        map[string]string{"icaro.managed": "preview-proxy", "icaro.proxy.config": want},
		Network:       Network,
		HostIP:        "127.0.0.1",
		HostPort:      o.HTTPPort,
		ContainerPort: 8080,
		HostConfig:    hc,
	})
	if err != nil {
		if strings.Contains(err.Error(), "Conflict") || strings.Contains(err.Error(), "already in use") {
			return nil // a concurrent ensure won
		}
		return fmt.Errorf("create %s: %w", ProxyName, err)
	}
	return e.StartContainer(ctx, id)
}

// writeRoute atomically writes one preview's route file.
func writeRoute(dir, name string, body []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name+".yaml"))
}

func removeRoute(dir, name string) error {
	err := os.Remove(filepath.Join(dir, name+".yaml"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
