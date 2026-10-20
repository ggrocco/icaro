// Package cli wires the icaro command tree. Commands are thin clients of
// the daemon (ADR 0001); business rules live in the service layer.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"icaro/internal/apiclient"
	"icaro/internal/config"
	"icaro/internal/daemon"
)

// version is set at build time via -ldflags.
var version = "dev"

// Version returns the build version.
func Version() string { return version }

type rootOpts struct {
	configPath string
	home       string
}

// ExitError carries a specific process exit code (e.g. an agent's).
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// commandFactories lets each command file register itself from init(), so
// adding a command never needs an edit here.
var commandFactories []func(*rootOpts) *cobra.Command

func register(f func(*rootOpts) *cobra.Command) { commandFactories = append(commandFactories, f) }

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cmd := newRootCmd()
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		var ee *ExitError
		if errors.As(err, &ee) {
			return ee.Code
		}
		var already *daemon.AlreadyRunningError
		if errors.As(err, &already) {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 3
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// Tree returns the full command tree (used by hack/gendocs to write the
// CLI reference).
func Tree() *cobra.Command { return newRootCmd() }

func newRootCmd() *cobra.Command {
	opts := &rootOpts{}
	root := &cobra.Command{
		Use:           "icaro",
		Short:         "Ícaro runs many coding agents in parallel, each in an isolated session",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.configPath, "config", "", "config file (default: $ICARO_CONFIG or ~/.icaro/config/icaro.yaml)")
	root.PersistentFlags().StringVar(&opts.home, "home", "", "state root (default: $ICARO_HOME or ~/.icaro)")
	root.AddCommand(newVersionCmd())
	for _, f := range commandFactories {
		root.AddCommand(f(opts))
	}
	return root
}

// loadConfig resolves configuration for commands that need it.
func (o *rootOpts) loadConfig() (*config.Config, error) {
	if o.home != "" {
		// The home flag also decides where the default config file lives.
		_ = os.Setenv("ICARO_HOME", o.home)
	}
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return nil, err
	}
	if o.home != "" {
		cfg.Home = o.home
	}
	return cfg, cfg.Validate()
}

// daemonArgs are the global flags forwarded to a background daemon.
func (o *rootOpts) daemonArgs() []string {
	var args []string
	if o.configPath != "" {
		args = append(args, "--config", o.configPath)
	}
	if o.home != "" {
		args = append(args, "--home", o.home)
	}
	return args
}

// client returns an API client for the daemon: TCP when daemon.url is set
// (token required), otherwise the local Unix socket, starting the daemon
// in the background when it is not running.
func (o *rootOpts) client(ctx context.Context) (*apiclient.Client, error) {
	cfg, err := o.loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Daemon.URL != "" {
		if cfg.Daemon.Token == "" {
			return nil, errors.New("daemon.url is set but no token: set daemon.token or ICARO_DAEMON__TOKEN")
		}
		return apiclient.New(cfg.Daemon.URL, cfg.Daemon.Token), nil
	}
	l := cfg.Layout()
	if err := daemon.EnsureRunning(ctx, l, o.daemonArgs()); err != nil {
		return nil, fmt.Errorf("reach daemon: %w", err)
	}
	return apiclient.NewWithHTTP(daemon.SocketBaseURL, "", daemon.HTTPClient(l, 0)), nil
}

func newVersionCmd() *cobra.Command {
	var long bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the icaro version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if long {
				fmt.Fprintf(cmd.OutOrStdout(), "icaro %s %s/%s %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
				return nil
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err
		},
	}
	cmd.Flags().BoolVar(&long, "long", false, "include platform and Go version")
	return cmd
}
