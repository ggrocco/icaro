// Package docker wraps the Docker Engine API behind the small interface the
// runner needs. The interface doubles as the allowlist for a socket proxy
// and makes the executor testable with a fake.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Spec describes a container to create.
type Spec struct {
	Name       string
	Image      string
	Cmd        []string
	Entrypoint []string
	Env        []string
	WorkingDir string
	User       string
	Labels     map[string]string
	HostConfig *container.HostConfig
}

// State is the subset of inspect data the runner uses.
type State struct {
	Exists    bool
	Running   bool
	ExitCode  int
	OOMKilled bool
	Labels    map[string]string
}

// Client is the engine API surface used by the runner.
type Client interface {
	Ping(ctx context.Context) error
	EnsureImage(ctx context.Context, ref string) error
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	CreateContainer(ctx context.Context, spec Spec) (id string, err error)
	StartContainer(ctx context.Context, id string) error
	// CopyIn writes a tar stream into the container at destDir.
	CopyIn(ctx context.Context, id, destDir string, tar io.Reader) error
	// CopyOut returns a tar stream of path (nil, ErrNotFound when missing).
	CopyOut(ctx context.Context, id, path string) (io.ReadCloser, error)
	// Logs streams multiplexed stdout/stderr; follow until exit when follow is true.
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	// Wait blocks until the container stops and returns its exit code.
	Wait(ctx context.Context, id string) (int, error)
	Inspect(ctx context.Context, id string) (State, error)
	Kill(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	// ListByLabel returns container ids carrying all the given labels.
	ListByLabel(ctx context.Context, labels map[string]string) ([]string, error)
}

// ErrNotFound reports a missing container, volume, image or path.
var ErrNotFound = errors.New("docker: not found")

// Mount describes a volume mount; kept here so callers do not import moby types.
type Mount = mount.Mount

// Engine is the real Docker implementation.
type Engine struct {
	cli *client.Client
}

// New connects to the engine. host overrides DOCKER_HOST when non-empty.
func New(host string) (*Engine, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	return &Engine{cli: cli}, nil
}

// Close releases the HTTP client.
func (e *Engine) Close() error { return e.cli.Close() }

func (e *Engine) Ping(ctx context.Context) error {
	_, err := e.cli.Ping(ctx, client.PingOptions{})
	return err
}

func (e *Engine) EnsureImage(ctx context.Context, ref string) error {
	if _, err := e.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	resp, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer func() { _ = resp.Close() }()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

func (e *Engine) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
	return err
}

func (e *Engine) RemoveVolume(ctx context.Context, name string) error {
	_, err := e.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *Engine) CreateContainer(ctx context.Context, spec Spec) (string, error) {
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
		NetworkingConfig: &network.NetworkingConfig{},
	})
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

func (e *Engine) StartContainer(ctx context.Context, id string) error {
	_, err := e.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (e *Engine) CopyIn(ctx context.Context, id, destDir string, tar io.Reader) error {
	_, err := e.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: destDir, Content: tar})
	return err
}

func (e *Engine) CopyOut(ctx context.Context, id, path string) (io.ReadCloser, error) {
	res, err := e.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: path})
	if cerrdefs.IsNotFound(err) || (err != nil && strings.Contains(err.Error(), "Could not find the file")) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return res.Content, nil
}

func (e *Engine) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	return e.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: follow})
}

func (e *Engine) Wait(ctx context.Context, id string) (int, error) {
	res := e.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case r := <-res.Result:
		if r.Error != nil {
			return int(r.StatusCode), errors.New(r.Error.Message)
		}
		return int(r.StatusCode), nil
	case err := <-res.Error:
		return -1, err
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (e *Engine) Inspect(ctx context.Context, id string) (State, error) {
	res, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	st := State{Exists: true}
	if res.Container.Config != nil {
		st.Labels = res.Container.Config.Labels
	}
	if res.Container.State != nil {
		st.Running = res.Container.State.Running
		st.ExitCode = res.Container.State.ExitCode
		st.OOMKilled = res.Container.State.OOMKilled
	}
	return st, nil
}

func (e *Engine) Kill(ctx context.Context, id string) error {
	_, err := e.cli.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: "SIGKILL"})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *Engine) RemoveContainer(ctx context.Context, id string) error {
	_, err := e.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *Engine) ListByLabel(ctx context.Context, labels map[string]string) ([]string, error) {
	f := client.Filters{}
	for k, v := range labels {
		f.Add("label", k+"="+v)
	}
	res, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Items))
	for _, c := range res.Items {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

var _ Client = (*Engine)(nil)
