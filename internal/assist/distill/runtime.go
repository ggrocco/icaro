package distill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"icaro/internal/container"
	"icaro/internal/container/docker"
)

// Docker is the part of the Docker adapter the managed runtime uses.
type Docker interface {
	EnsureImage(ctx context.Context, ref string) error
	CreateContainer(ctx context.Context, spec docker.Spec) (string, error)
	StartContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	ListByLabel(ctx context.Context, labels map[string]string) ([]string, error)
}

// Managed runs the model in a llama-server container with no network,
// started for a batch of jobs and removed after it (ADR 0036). It is CPU
// only; GPU users point memory.distill.endpoint at their own server.
type Managed struct {
	Docker Docker
	Models Store
	// SocketDir is the host directory the server's socket appears in.
	SocketDir string
	// User is the uid:gid the server runs as (the daemon's, so it can
	// reach the socket).
	User string
	CPUs float64
	// Name is the container name; Key labels it, so leftovers of this
	// state root are found after a crash.
	Name, Key string
	Image     string
	// ReadyTimeout bounds the model load (default 3 minutes).
	ReadyTimeout time.Duration
	// Poll is how often readiness is checked (default 500ms).
	Poll time.Duration
}

// Start launches the server for m and returns a client and a stop
// function that removes the container.
func (r Managed) Start(ctx context.Context, m Model) (*Client, func(), error) {
	r.cleanup(ctx)
	if err := os.MkdirAll(r.SocketDir, 0o700); err != nil {
		return nil, nil, err
	}
	sock := filepath.Join(r.SocketDir, container.DistillSocketName)
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	spec := container.DistillSpec{
		Name: r.Name, Image: r.Image, User: r.User, ModelsDir: r.Models.Dir, SocketDir: r.SocketDir, ModelFile: m.File,
		Threads: max(int(r.CPUs), 1), NanoCPUs: int64(r.CPUs * 1e9), MemoryBytes: container.DistillMemory(m.Size),
		Labels: map[string]string{container.LabelDistill: r.Key},
	}.Build()
	if err := r.Docker.EnsureImage(ctx, spec.Image); err != nil {
		return nil, nil, fmt.Errorf("model server image: %w", err)
	}
	id, err := r.Docker.CreateContainer(ctx, spec)
	if err != nil {
		return nil, nil, fmt.Errorf("create the model server: %w", err)
	}
	stop := func() { _ = r.Docker.RemoveContainer(context.WithoutCancel(ctx), id) }
	if err := r.Docker.StartContainer(ctx, id); err != nil {
		stop()
		return nil, nil, fmt.Errorf("start the model server: %w", err)
	}
	timeout := r.ReadyTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	poll := r.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	c := UnixClient(sock)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.WaitReady(rctx, poll); err != nil {
		stop()
		return nil, nil, err
	}
	return c, stop, nil
}

// cleanup removes model servers a crash left behind.
func (r Managed) cleanup(ctx context.Context) {
	ids, err := r.Docker.ListByLabel(ctx, map[string]string{container.LabelDistill: r.Key})
	if err != nil {
		return
	}
	for _, id := range ids {
		_ = r.Docker.RemoveContainer(ctx, id)
	}
}
