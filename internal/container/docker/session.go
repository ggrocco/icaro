package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// The methods in this file serve the session spine (Phase 1): networks
// for egress confinement, overlay volumes for profile homes, image digest
// pinning and long-lived agent containers.

// NetSpec attaches a new container to a network with DNS aliases.
type NetSpec struct {
	Network string
	Aliases []string
}

// CreateContainerOn creates a container attached to net (instead of the
// default bridge) with optional aliases.
func (e *Engine) CreateContainerOn(ctx context.Context, spec Spec, net NetSpec) (string, error) {
	nc := &network.NetworkingConfig{}
	if net.Network != "" {
		nc.EndpointsConfig = map[string]*network.EndpointSettings{net.Network: {Aliases: net.Aliases}}
		if spec.HostConfig != nil {
			spec.HostConfig.NetworkMode = container.NetworkMode(net.Network)
		}
	}
	res, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:      spec.Image,
			Cmd:        spec.Cmd,
			Entrypoint: spec.Entrypoint,
			Env:        spec.Env,
			WorkingDir: spec.WorkingDir,
			User:       spec.User,
			Labels:     spec.Labels,
		},
		HostConfig:       spec.HostConfig,
		NetworkingConfig: nc,
	})
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

// NetworkInfo is the subset of network inspect data the egress checks use.
type NetworkInfo struct {
	Name     string
	Internal bool
	Labels   map[string]string
	// Containers counts the endpoints attached to the network.
	Containers int
}

// NetworkMismatch reports why an existing network cannot serve a request
// for an (internal) network carrying labels, or nil when it can. A
// non-internal network would give its containers a direct route out, and
// one without our labels was not created by Ícaro, so other containers
// may share it (and answer for the egress proxy's alias).
func NetworkMismatch(n NetworkInfo, internal bool, labels map[string]string) error {
	if internal && !n.Internal {
		return fmt.Errorf("network %s exists but is not internal, so containers on it could bypass the egress proxy; remove it (`docker network rm %s`) and retry", n.Name, n.Name)
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if n.Labels[k] != labels[k] {
			return fmt.Errorf("network %s exists but was not created by Ícaro (label %s=%q, want %q); remove it (`docker network rm %s`) and retry", n.Name, k, n.Labels[k], labels[k], n.Name)
		}
	}
	return nil
}

// CreateNetwork creates a network when missing. internal=true creates a
// network with no route outside the host. An existing network is reused
// only when it matches (NetworkMismatch): egress confinement must not
// fail open onto a network someone else created.
func (e *Engine) CreateNetwork(ctx context.Context, name string, internal bool, labels map[string]string) error {
	n, err := e.InspectNetwork(ctx, name)
	if err == nil {
		return NetworkMismatch(n, internal, labels)
	} else if !IsNotFound(err) {
		return err
	}
	_, err = e.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: internal, Labels: labels})
	if cerrdefs.IsConflict(err) {
		// Created concurrently: check what won the race.
		if n, err = e.InspectNetwork(ctx, name); err != nil {
			return err
		}
		return NetworkMismatch(n, internal, labels)
	}
	return err
}

// InspectNetwork returns a network's settings (ErrNotFound when missing).
func (e *Engine) InspectNetwork(ctx context.Context, name string) (NetworkInfo, error) {
	res, err := e.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return NetworkInfo{}, ErrNotFound
	}
	if err != nil {
		return NetworkInfo{}, err
	}
	return NetworkInfo{Name: res.Network.Name, Internal: res.Network.Internal, Labels: res.Network.Labels, Containers: len(res.Network.Containers)}, nil
}

// ListNetworks returns the networks whose name starts with prefix.
func (e *Engine) ListNetworks(ctx context.Context, prefix string) ([]NetworkInfo, error) {
	res, err := e.cli.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("name", prefix)})
	if err != nil {
		return nil, err
	}
	var out []NetworkInfo
	for _, s := range res.Items {
		if !strings.HasPrefix(s.Name, prefix) {
			continue // the name filter matches substrings
		}
		n, err := e.InspectNetwork(ctx, s.Name)
		if IsNotFound(err) {
			continue // removed meanwhile
		}
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// ConnectNetwork attaches a container to an extra network.
func (e *Engine) ConnectNetwork(ctx context.Context, networkName, containerID string, aliases []string) error {
	_, err := e.cli.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{
		Container: containerID, EndpointConfig: &network.EndpointSettings{Aliases: aliases},
	})
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// ContainerIPs returns the container's IPv4 address per network.
func (e *Engine) ContainerIPs(ctx context.Context, id string) (map[string]string, error) {
	res, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if res.Container.NetworkSettings != nil {
		for name, ep := range res.Container.NetworkSettings.Networks {
			if ep != nil && ep.IPAddress.IsValid() {
				out[name] = ep.IPAddress.String()
			}
		}
	}
	return out, nil
}

// CreateVolumeOpts creates a volume with a driver and options (e.g. the
// local driver with type=overlay).
func (e *Engine) CreateVolumeOpts(ctx context.Context, name, driver string, opts, labels map[string]string) error {
	_, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Driver: driver, DriverOpts: opts, Labels: labels})
	return err
}

// ImageDigest returns the repo digest (name@sha256:...) of a local image,
// or its image ID when it has no repo digest (locally built/imported).
func (e *Engine) ImageDigest(ctx context.Context, ref string) (string, error) {
	res, err := e.cli.ImageInspect(ctx, ref)
	if cerrdefs.IsNotFound(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if len(res.RepoDigests) > 0 {
		return res.RepoDigests[0], nil
	}
	return res.ID, nil
}

// PullImage pulls ref even when a local copy exists.
func (e *Engine) PullImage(ctx context.Context, ref string) error {
	resp, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer func() { _ = resp.Close() }()
	return resp.Wait(ctx)
}

// ImportImage creates an image named ref (repo:tag) from a root
// filesystem tarball.
func (e *Engine) ImportImage(ctx context.Context, rootfs io.Reader, ref string, changes []string) error {
	repo, tag := ref, ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	rc, err := e.cli.ImageImport(ctx, client.ImageImportSource{Source: rootfs, SourceName: "-"}, repo, client.ImageImportOptions{Tag: tag, Changes: changes}) //nolint:staticcheck // Tag is the only way to name an import

	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(io.Discard, rc)
	return err
}

// StopContainer stops a container gracefully (SIGTERM, then SIGKILL after
// timeout seconds); missing is not an error.
func (e *Engine) StopContainer(ctx context.Context, id string, timeout int) error {
	_, err := e.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// IsNotFound reports a missing object from any Engine call.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) || cerrdefs.IsNotFound(err) }
