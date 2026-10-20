package docker

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// EnsureNetwork creates a bridge network when it does not exist and
// reports whether it was created.
func (e *Engine) EnsureNetwork(ctx context.Context, name string, labels map[string]string) (bool, error) {
	ok, err := e.NetworkExists(ctx, name)
	if err != nil || ok {
		return false, err
	}
	_, err = e.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: labels})
	if cerrdefs.IsConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create network %s: %w", name, err)
	}
	return true, nil
}

// NetworkExists reports whether a network with that name exists.
func (e *Engine) NetworkExists(ctx context.Context, name string) (bool, error) {
	_, err := e.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RemoveNetwork deletes a network; a missing one is not an error.
func (e *Engine) RemoveNetwork(ctx context.Context, name string) error {
	_, err := e.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// Restart restarts a container (used to make a proxy re-read its config
// where bind-mount file events are unreliable).
func (e *Engine) Restart(ctx context.Context, id string) error {
	_, err := e.cli.ContainerRestart(ctx, id, client.ContainerRestartOptions{})
	return err
}

// ProxySpec describes a long-running proxy container published on one
// host port.
type ProxySpec struct {
	Name          string
	Image         string
	Cmd           []string
	User          string
	Labels        map[string]string
	Network       string
	HostIP        string
	HostPort      int
	ContainerPort int
	HostConfig    *container.HostConfig // baseline; ports and network are added
}

// CreateProxy creates (does not start) a proxy container.
func (e *Engine) CreateProxy(ctx context.Context, s ProxySpec) (string, error) {
	port, err := network.ParsePort(strconv.Itoa(s.ContainerPort) + "/tcp")
	if err != nil {
		return "", err
	}
	ip, err := netip.ParseAddr(s.HostIP)
	if err != nil {
		return "", fmt.Errorf("proxy host ip: %w", err)
	}
	hc := s.HostConfig
	if hc == nil {
		hc = &container.HostConfig{}
	}
	hc.NetworkMode = container.NetworkMode(s.Network)
	hc.PortBindings = network.PortMap{port: {{HostIP: ip, HostPort: strconv.Itoa(s.HostPort)}}}
	hc.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	res, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: s.Name,
		Config: &container.Config{
			Image:        s.Image,
			Cmd:          s.Cmd,
			User:         s.User,
			Labels:       s.Labels,
			ExposedPorts: network.PortSet{port: {}},
		},
		HostConfig: hc,
	})
	if err != nil {
		return "", err
	}
	return res.ID, nil
}
