package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/mount"

	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
)

// Paths inside an agent container.
const (
	WorkspacePath = "/workspace"
	HomePath      = "/home/icaro"
	// IcaroPath holds the session's MCP socket (/icaro/mcp.sock).
	IcaroPath = "/icaro"
)

// Labels on session containers.
const (
	LabelSession = "icaro.session"
	LabelKind    = "icaro.kind"
	LabelProfile = "icaro.profile"
)

// KeepAlive keeps a session container running so commands can be exec'd
// into it; tini (Init) reaps children and forwards signals.
var KeepAlive = []string{"sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"}

// HomeMount is the profile home for one session: a Docker volume (overlay
// mode) or a host directory (copy mode).
type HomeMount struct {
	Volume string
	Dir    string
}

// AgentSpec describes a session's agent container.
type AgentSpec struct {
	Name    string
	Image   string
	User    string // uid:gid
	Env     []string
	Labels  map[string]string
	Command []string // default KeepAlive

	// Workspace is the host worktree mounted at /workspace.
	Workspace string
	// GitDir is the repository's common .git directory, mounted at its
	// host path so the worktree's .git file resolves. Its hooks/ and
	// config are mounted read-only so an agent cannot plant code that the
	// host's git would later run (ADR 0011).
	GitDir string
	Home   HomeMount
	// IcaroDir is the host directory mounted at /icaro (MCP socket).
	IcaroDir string
	// Extra are policy-checked extra mounts.
	Extra   []Mount
	Limits  sandbox.Limits
	Runtime string
	Egress  Attachment
}

// Build returns the container spec and its network.
func (a AgentSpec) Build() (docker.Spec, docker.NetSpec) {
	var mounts []mount.Mount
	bind := func(src, dst string, ro bool) {
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: src, Target: dst, ReadOnly: ro})
	}
	if a.Workspace != "" {
		bind(a.Workspace, WorkspacePath, false)
	}
	if a.GitDir != "" {
		bind(a.GitDir, a.GitDir, false)
		for _, sub := range []string{"hooks", "config"} {
			p := filepath.Join(a.GitDir, sub)
			if _, err := os.Stat(p); err == nil {
				bind(p, p, true)
			}
		}
	}
	switch {
	case a.Home.Volume != "":
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: a.Home.Volume, Target: HomePath})
	case a.Home.Dir != "":
		bind(a.Home.Dir, HomePath, false)
	}
	if a.IcaroDir != "" {
		bind(a.IcaroDir, IcaroPath, false)
	}
	for _, m := range a.Extra {
		bind(m.Source, m.Target, m.ReadOnly)
		for _, mask := range m.Masks {
			mounts = append(mounts, mount.Mount{Type: mount.TypeTmpfs, Target: mask, TmpfsOptions: &mount.TmpfsOptions{SizeBytes: 1 << 20}})
		}
	}
	hc := sandbox.Baseline(sandbox.BaselineOptions{
		Limits: a.Limits, Mounts: mounts, NetworkMode: a.Egress.Network,
		TmpfsHome: a.Home.Volume == "" && a.Home.Dir == "", Runtime: a.Runtime,
	})
	cmd := a.Command
	if len(cmd) == 0 {
		cmd = KeepAlive
	}
	env := append([]string{"HOME=" + HomePath, "TERM=xterm-256color", "LANG=C.UTF-8"}, a.Egress.Env...)
	env = append(env, a.Env...)
	spec := docker.Spec{
		Name: a.Name, Image: a.Image, User: a.User, Env: env, Labels: a.Labels,
		Entrypoint: cmd, WorkingDir: WorkspacePath, HostConfig: hc,
	}
	return spec, docker.NetSpec{Network: a.Egress.Network}
}

// ExecOptions describe a `docker exec` into a session container.
type ExecOptions struct {
	Container   string
	User        string
	Workdir     string
	TTY         bool
	Interactive bool
	Env         []string
	Cmd         []string
}

// ExecArgs builds the docker CLI arguments for o.
func ExecArgs(o ExecOptions) []string {
	args := []string{"exec"}
	if o.Interactive {
		args = append(args, "-i")
	}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.User != "" {
		args = append(args, "-u", o.User)
	}
	wd := o.Workdir
	if wd == "" {
		wd = WorkspacePath
	}
	args = append(args, "-w", wd)
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	args = append(args, o.Container)
	return append(args, o.Cmd...)
}

// ExitCode maps the error of a finished docker CLI process to the exit
// code to propagate: the command's own code, or 1 for failures to run.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if ok := asExit(err, &ee); ok {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
	}
	return 1
}

func asExit(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ShellCommand is the default interactive shell: bash when present.
var ShellCommand = []string{"sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi"}

// QuoteArgs renders args for display.
func QuoteArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"$;&|") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}
