package preview

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Compose runs `docker compose`. The CLI is used instead of the SDK
// because it is the reference implementation of compose semantics
// (05-engineering: CLI subprocess initially).
type Compose interface {
	// Config returns the normalized model (`docker compose config --format
	// json`) under the project name Up uses, so names compose derives (the
	// default network) are the ones the stack will really get.
	Config(ctx context.Context, project, dir string, files []string) ([]byte, error)
	Up(ctx context.Context, project, dir string, files []string) error
	// Down removes the project's containers, networks and volumes; a
	// project that does not exist is not an error.
	Down(ctx context.Context, project string) error
	// Running counts the project's running containers.
	Running(ctx context.Context, project string) (int, error)
	Logs(ctx context.Context, project string, tail int, w io.Writer) error
}

// CLI is the real Compose implementation.
type CLI struct {
	// Binary defaults to "docker".
	Binary string
	// Env is appended to the process environment (e.g. DOCKER_HOST).
	Env []string
}

func (c CLI) cmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	bin := c.Binary
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"compose"}, args...)...) //nolint:gosec // fixed binary, args built here
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), c.Env...)
	return cmd
}

// ComposeError is a failed docker compose command. Its message carries
// compose's stderr, which is about the user's setup (Docker not running,
// a bad compose file), so the API passes it on.
type ComposeError struct {
	msg string
	err error
}

func (e *ComposeError) Error() string { return e.msg }
func (e *ComposeError) Unwrap() error { return e.err }

func run(cmd *exec.Cmd) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return stdout.Bytes(), &ComposeError{msg: fmt.Sprintf("docker %s: %v: %s", strings.Join(cmd.Args[1:min(len(cmd.Args), 4)], " "), err, msg), err: err}
	}
	return stdout.Bytes(), nil
}

func fileArgs(files []string) []string {
	var out []string
	for _, f := range files {
		out = append(out, "-f", f)
	}
	return out
}

// Config implements Compose.
func (c CLI) Config(ctx context.Context, project, dir string, files []string) ([]byte, error) {
	args := append([]string{"-p", project}, fileArgs(files)...)
	args = append(args, "--project-directory", dir, "config", "--format", "json")
	return run(c.cmd(ctx, dir, args...))
}

// Up implements Compose.
func (c CLI) Up(ctx context.Context, project, dir string, files []string) error {
	args := append([]string{"-p", project}, fileArgs(files)...)
	args = append(args, "--project-directory", dir, "up", "-d", "--build", "--remove-orphans", "--wait", "--wait-timeout", "120")
	_, err := run(c.cmd(ctx, dir, args...))
	if err != nil && strings.Contains(err.Error(), "unknown flag: --wait") {
		args = args[:len(args)-3]
		_, err = run(c.cmd(ctx, dir, args...))
	}
	return err
}

// projectOnly runs a project-scoped command (no -f) from an empty
// directory, so compose cannot pick up an unrelated compose file from the
// working directory.
func (c CLI) projectOnly(ctx context.Context, args ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "icaro-compose-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return run(c.cmd(ctx, dir, args...))
}

// Down implements Compose.
func (c CLI) Down(ctx context.Context, project string) error {
	_, err := c.projectOnly(ctx, "-p", project, "down", "-v", "--remove-orphans", "--timeout", "10")
	if err != nil && (strings.Contains(err.Error(), "no configuration file") || strings.Contains(err.Error(), "not found")) {
		return nil
	}
	return err
}

// Running implements Compose.
func (c CLI) Running(ctx context.Context, project string) (int, error) {
	out, err := c.projectOnly(ctx, "-p", project, "ps", "-q", "--status", "running")
	if err != nil {
		if strings.Contains(err.Error(), "no configuration file") {
			return 0, nil
		}
		return 0, err
	}
	return len(strings.Fields(string(out))), nil
}

// Logs implements Compose.
func (c CLI) Logs(ctx context.Context, project string, tail int, w io.Writer) error {
	if tail <= 0 {
		tail = 200
	}
	dir, err := os.MkdirTemp("", "icaro-compose-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cmd := c.cmd(ctx, dir, "-p", project, "logs", "--no-color", "--tail", fmt.Sprint(tail))
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

var _ Compose = CLI{}
