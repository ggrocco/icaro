package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"icaro/internal/apiclient"
	"icaro/internal/app"
	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
	"icaro/internal/doctor"
	"icaro/internal/session"
	"icaro/internal/store"
)

func init() {
	register(newSessionCmd)
	register(newShCmd)
	register(func(o *rootOpts) *cobra.Command { return newAgentCmd(o, "claude") })
	register(func(o *rootOpts) *cobra.Command { return newAgentCmd(o, "codex") })
	register(newLoginCmd)
	extraDoctorChecks = append(extraDoctorChecks, func(_ context.Context, cfg *config.Config, st *store.Store, eng *docker.Engine) []doctor.Check {
		return app.SessionChecks(cfg, st, eng)
	})
}

func printSessions(w io.Writer, ss []*session.Session) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tPROFILE\tSTATUS\tBRANCH\tWORKTREE")
	for _, s := range ss {
		status := s.Status
		if s.Error != "" && s.Status != session.StatusRunning {
			status += " (" + s.Error + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Kind, s.Profile, status, dash(s.Branch), s.WorktreePath)
	}
	return tw.Flush()
}

func remoteOrOrigin(r string) string {
	if r == "" {
		return "origin"
	}
	return r
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newSessionCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "session", Aliases: []string{"sessions"}, Short: "Manage sessions: worktree + isolated agent container"}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")
	show := func(cmd *cobra.Command, s *session.Session) error {
		if asJSON {
			return printJSON(cmd.OutOrStdout(), s)
		}
		return printSessions(cmd.OutOrStdout(), []*session.Session{s})
	}

	var req session.CreateRequest
	create := &cobra.Command{
		Use:   "create [NAME]",
		Short: "Create a worktree on branch icaro/NAME and start an agent session for it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				req.Name = args[0]
			}
			if req.RepoPath == "" {
				req.RepoPath, _ = os.Getwd()
			}
			abs, err := filepath.Abs(req.RepoPath)
			if err != nil {
				return err
			}
			req.RepoPath = abs
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			s, err := c.CreateSession(cmd.Context(), req)
			if err != nil {
				return err
			}
			if err := show(cmd, s); err != nil {
				return err
			}
			if s.Status == session.StatusQueued {
				fmt.Fprintln(cmd.ErrOrStderr(), "queued: the active-session cap is reached; it starts when another session stops")
			}
			return nil
		},
	}
	create.Flags().StringVar(&req.RepoPath, "repo", "", "repository path (default: current directory)")
	create.Flags().StringVar(&req.Profile, "profile", "", "profile (default: sessions.default_profile)")
	create.Flags().StringVar(&req.Base, "base", "", "commit-ish the new branch starts from (default: HEAD)")

	var project, kind, status string
	list := &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			ss, err := c.ListSessions(cmd.Context(), project, kind, status)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), ss)
			}
			return printSessions(cmd.OutOrStdout(), ss)
		},
	}
	list.Flags().StringVar(&project, "project", "", "filter by project name")
	list.Flags().StringVar(&kind, "kind", "", "agent | run")
	list.Flags().StringVar(&status, "status", "", "filter by status")

	one := func(use, short string, fn func(ctx context.Context, c *apiclient.Client, id string) (*session.Session, error)) *cobra.Command {
		return &cobra.Command{
			Use: use + " ID", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := opts.client(cmd.Context())
				if err != nil {
					return err
				}
				s, err := fn(cmd.Context(), c, args[0])
				if err != nil {
					return err
				}
				return show(cmd, s)
			},
		}
	}
	get := one("get", "Show a session", func(ctx context.Context, c *apiclient.Client, id string) (*session.Session, error) {
		return c.GetSession(ctx, id)
	})
	stop := one("stop", "Stop a session's container (worktree and home are kept)", func(ctx context.Context, c *apiclient.Client, id string) (*session.Session, error) {
		return c.StopSession(ctx, id)
	})
	start := one("start", "Restart a stopped or orphaned session", func(ctx context.Context, c *apiclient.Client, id string) (*session.Session, error) {
		return c.StartSession(ctx, id)
	})

	var yes, keep bool
	rm := &cobra.Command{
		Use:   "rm ID",
		Short: "Remove a session and its worktree (the branch is kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				what := "and its worktree"
				if keep {
					what = "(keeping its worktree)"
				}
				ok, err := confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), fmt.Sprintf("Remove session %s %s? Uncommitted changes are lost.", args[0], what))
				if err != nil || !ok {
					return err
				}
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			return c.RemoveSession(cmd.Context(), args[0], keep)
		},
	}
	rm.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	rm.Flags().BoolVar(&keep, "keep-worktree", false, "keep the worktree directory")

	var pushReq session.PushRequest
	var pushYes bool
	push := &cobra.Command{
		Use:   "push ID",
		Short: "Push the session's branch from the host with your git credentials",
		Long: "Push the session's own branch (icaro/NAME) to a remote configured in the repository, from the host, " +
			"with your git credentials, so agent containers never hold any. Only that branch is pushed (no tags, no " +
			"submodules); host git runs with hooks and fsmonitor disabled and the file transport refused, because the " +
			"agent controlled the worktree. --force-with-lease overwrites the remote branch only while it still points " +
			"where your remote-tracking ref says (fetch first); a plain force push is never done.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if pushReq.ForceWithLease && !pushYes {
				ok, err := confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), fmt.Sprintf("Overwrite the remote branch of session %s on %s if it moved?", args[0], remoteOrOrigin(pushReq.Remote)))
				if err != nil || !ok {
					return err
				}
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.PushSession(cmd.Context(), args[0], pushReq)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			verb := "pushed"
			if res.Forced {
				verb = "force-pushed (with lease)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s) to %s\n", verb, res.Branch, short12(res.Commit), res.Remote)
			return nil
		},
	}
	push.Flags().StringVar(&pushReq.Remote, "remote", "origin", "name of a remote configured in the repository")
	push.Flags().BoolVar(&pushReq.ForceWithLease, "force-with-lease", false, "overwrite the remote branch if it still matches your remote-tracking ref")
	push.Flags().BoolVar(&pushYes, "yes", false, "do not ask before --force-with-lease")

	attach := &cobra.Command{
		Use:   "attach ID [-- CMD...]",
		Short: "Open the session's agent (or a shell) with a TTY",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			info, err := c.SessionExec(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			run := args[1:]
			if len(run) == 0 {
				run = info.AgentCmd
			}
			return execInSession(cmd.Context(), opts, info, run)
		},
	}

	projects := &cobra.Command{
		Use:   "projects",
		Short: "List projects (repositories with sessions)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			ps, err := c.ListProjects(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tROOT\tREMOTE")
			for _, p := range ps {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.RootPath, dash(p.Remote))
			}
			return tw.Flush()
		},
	}
	cmd.AddCommand(create, list, get, stop, start, rm, push, attach, projects)
	return cmd
}

// execInSession runs argv in the session container with TTY passthrough
// and propagates the exit code.
func execInSession(ctx context.Context, opts *rootOpts, info *session.ExecInfo, argv []string) error {
	cfg, err := opts.loadConfig()
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		argv = container.ShellCommand
	}
	tty := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	args := container.ExecArgs(container.ExecOptions{
		Container: info.ContainerID, User: info.User, Workdir: info.Workdir, TTY: tty, Interactive: true, Cmd: argv,
	})
	return runDockerCLI(ctx, cfg.Docker.Host, args)
}

// dockerEnv is the docker CLI's environment: environ, pointed at host
// (config docker.host) when set, so the CLI reaches the same engine as
// the daemon. A later DOCKER_HOST entry wins, and DOCKER_HOST overrides
// the CLI's current context.
func dockerEnv(environ []string, host string) []string {
	if host == "" {
		return environ
	}
	return append(append([]string{}, environ...), "DOCKER_HOST="+host)
}

// runDockerCLI runs the docker CLI against host ("" keeps the user's
// environment) attached to our stdio.
func runDockerCLI(ctx context.Context, host string, args []string) error {
	c := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // argv built by container.ExecArgs
	c.Env = dockerEnv(os.Environ(), host)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: container.ExitCode(err)}
	}
	return err
}

// ensureForCwd returns a running session for the current repository.
func ensureForCwd(ctx context.Context, c *apiclient.Client, profile string) (*session.ExecInfo, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	s, err := c.EnsureSession(ctx, session.CreateRequest{RepoPath: wd, Profile: profile})
	if err != nil {
		return nil, err
	}
	if s.Status != session.StatusRunning {
		return nil, fmt.Errorf("session %s is %s %s", s.ID, s.Status, s.Error)
	}
	return c.SessionExec(ctx, s.ID)
}

func newShCmd(opts *rootOpts) *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "sh [SESSION] [-- CMD...]",
		Short: "Open a shell in a session (the current repository's, created on demand)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			var info *session.ExecInfo
			dash := cmd.ArgsLenAtDash()
			if len(args) > 0 && dash != 0 {
				info, err = c.SessionExec(cmd.Context(), args[0])
				args = args[1:]
			} else {
				info, err = ensureForCwd(cmd.Context(), c, profile)
			}
			if err != nil {
				return err
			}
			return execInSession(cmd.Context(), opts, info, args)
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "profile for a new session (default: sessions.default_profile)")
	return cmd
}

// newAgentCmd runs an agent CLI inside the current repository's session.
// All arguments pass through to the agent unchanged.
func newAgentCmd(opts *rootOpts, agent string) *cobra.Command {
	return &cobra.Command{
		Use:                agent + " [ARGS...]",
		Short:              "Run " + agent + " in an isolated session for the current repository",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := os.Getenv("ICARO_PROFILE")
			if profile == "" {
				profile = agent
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			info, err := ensureForCwd(cmd.Context(), c, profile)
			if err != nil {
				return err
			}
			if len(info.AgentCmd) == 0 {
				return fmt.Errorf("profile %s has no agent; use `icaro sh`", profile)
			}
			return execInSession(cmd.Context(), opts, info, append(append([]string{}, info.AgentCmd...), args...))
		},
	}
}

func newLoginCmd(opts *rootOpts) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "login PROFILE",
		Short: "Log the profile's agent in once; the login seeds every session of the profile",
		Long: "Log the profile's agent in once; the login seeds every session of the profile.\n\n" +
			"The login rewrites the profile's home seed, which running sessions may be using, so it is refused " +
			"while sessions of the profile are queued, starting, or running on overlay homes; stop them first, or pass --force. " +
			"While the login runs, new sessions of the profile wait and start when it ends.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			info, err := c.PrepareLogin(cmd.Context(), args[0], force)
			if err != nil {
				return err
			}
			// Sessions of the profile wait while the login runs; let them
			// go however it ends (the daemon also drops a stale lease).
			defer func() { _ = c.EndLogin(context.WithoutCancel(cmd.Context()), args[0]) }()
			seccomp := filepath.Join(cfg.Layout().RunDir(), "seccomp.json")
			if err := os.WriteFile(seccomp, []byte(sandbox.SeccompProfile()), 0o600); err != nil {
				return err
			}
			dargs := loginArgs(info, seccomp, isTerminal(os.Stdin))
			fmt.Fprintf(cmd.ErrOrStderr(), "logging in %s; the seed lives in %s (never your host ~/.claude)\n", info.Profile, info.SeedDir)
			return runDockerCLI(cmd.Context(), cfg.Docker.Host, dargs)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "log in even while sessions of the profile use its home seed")
	return cmd
}

// loginArgs builds `docker run` for a login under the container baseline,
// with the seed mounted read-write and egress confined to the profile.
func loginArgs(info *apiclient.LoginInfo, seccompFile string, tty bool) []string {
	args := []string{"run", "--rm", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args,
		"--user", info.User, "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=256m",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--security-opt", "seccomp="+seccompFile,
		"--pids-limit", "256", "--memory", "1g", "--network", info.Egress.Network,
		"-e", "HOME="+container.HomePath, "-v", info.SeedDir+":"+container.HomePath, "-w", container.HomePath,
	)
	for _, e := range info.Egress.Env {
		args = append(args, "-e", e)
	}
	args = append(args, info.Image)
	return append(args, info.Command...)
}
