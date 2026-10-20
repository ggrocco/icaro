package docker

import (
	"context"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// ContainerStarted reports whether a container exists and was ever
// started. Re-attach uses it to tell a container that crashed the daemon
// between create and start (never started) from one that ran.
func (e *Engine) ContainerStarted(ctx context.Context, id string) (exists, started bool, err error) {
	res, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if res.Container.State == nil {
		return true, false, nil
	}
	s := res.Container.State.StartedAt
	return true, s != "" && !strings.HasPrefix(s, "0001-01-01"), nil
}

// ContainerStarted implements the same probe for the fake engine.
func (f *Fake) ContainerStarted(_ context.Context, id string) (exists, started bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Containers[id]
	if !ok || c.Removed {
		return false, false, nil
	}
	return true, c.Started, nil
}
