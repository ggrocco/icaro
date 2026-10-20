package container

import (
	"context"
	"io"

	"icaro/internal/container/docker"
)

// Engine is the Docker surface this package needs; *docker.Engine
// implements it and tests use a fake.
type Engine interface {
	EnsureImage(ctx context.Context, ref string) error
	PullImage(ctx context.Context, ref string) error
	ImageDigest(ctx context.Context, ref string) (string, error)
	ImportImage(ctx context.Context, rootfs io.Reader, ref string, changes []string) error
	EnsureNetwork(ctx context.Context, name string, internal bool, labels map[string]string) error
	ConnectNetwork(ctx context.Context, network, containerID string, aliases []string) error
	CreateContainerOn(ctx context.Context, spec docker.Spec, net docker.NetSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	Inspect(ctx context.Context, id string) (docker.State, error)
	RemoveContainer(ctx context.Context, id string) error
	ListByLabel(ctx context.Context, labels map[string]string) ([]string, error)
}

var _ Engine = (*docker.Engine)(nil)
