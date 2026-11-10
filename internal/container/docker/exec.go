package docker

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// ExecOptions is one command run inside a running container.
type ExecOptions struct {
	Cmd     []string
	User    string
	Workdir string
	Env     []string
	// Stdin is written to the command's standard input, which is then
	// closed.
	Stdin []byte
}

// Exec runs o inside container id, streams its stdout and stderr to the
// writers, and returns its exit code. Cancelling ctx closes the stream;
// the process itself keeps running until the caller stops it (Docker has
// no way to signal an exec).
func (e *Engine) Exec(ctx context.Context, id string, o ExecOptions, stdout, stderr io.Writer) (int, error) {
	created, err := e.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: o.User, WorkingDir: o.Workdir, Env: o.Env, Cmd: o.Cmd,
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return -1, err
	}
	att, err := e.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, err
	}
	defer att.Close()
	copied := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, att.Reader)
		copied <- err
	}()
	if len(o.Stdin) > 0 {
		if _, err := att.Conn.Write(o.Stdin); err != nil {
			return -1, err
		}
	}
	if err := att.CloseWrite(); err != nil {
		return -1, err
	}
	select {
	case err := <-copied:
		if err != nil && !errors.Is(err, io.EOF) {
			return -1, err
		}
	case <-ctx.Done():
		att.Close()
		return -1, ctx.Err()
	}
	// The stream ends when the process exits; its exit code can lag a
	// moment behind.
	for range 50 {
		res, err := e.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return -1, err
		}
		if !res.Running {
			return res.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return -1, errors.New("docker: exec still running after its output ended")
}

// FakeExec is one exec the fake engine ran.
type FakeExec struct {
	Container string
	Options   ExecOptions
}

// Exec runs OnExec for a started container (exit 0 when unset).
func (f *Fake) Exec(ctx context.Context, id string, o ExecOptions, stdout, stderr io.Writer) (int, error) {
	f.acquire()
	c, ok := f.Containers[id]
	running := ok && c.Started && !c.Removed && !c.Killed
	if running {
		f.Execs = append(f.Execs, FakeExec{Container: id, Options: o})
	}
	on := f.OnExec
	f.release()
	if !running {
		return -1, ErrNotFound
	}
	if on == nil {
		return 0, nil
	}
	return on(ctx, o, stdout, stderr)
}
