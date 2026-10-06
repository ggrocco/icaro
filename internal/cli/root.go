// Package cli wires the icaro command tree.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"icaro/internal/config"
)

// version is set at build time via -ldflags.
var version = "dev"

type rootOpts struct {
	configPath string
	dataDir    string
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cmd := newRootCmd()
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	opts := &rootOpts{}
	root := &cobra.Command{
		Use:           "icaro",
		Short:         "Icaro runs workflows of containerized steps",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.configPath, "config", "", "config file (default: $ICARO_CONFIG, ./icaro.yaml, $XDG_CONFIG_HOME/icaro/icaro.yaml)")
	root.PersistentFlags().StringVar(&opts.dataDir, "data-dir", "", "data directory (default from config)")

	root.AddCommand(
		newVersionCmd(), newConfigCmd(opts), newInitCmd(opts), newMigrateCmd(opts), newTokenCmd(opts),
		newServeCmd(opts), newWorkflowCmd(opts), newRunCmd(opts), newRunsCmd(opts), newConnectionCmd(opts), newSchemaCmd(),
	)
	return root
}

// loadConfig resolves configuration for commands that need it.
func (o *rootOpts) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return nil, err
	}
	if o.dataDir != "" {
		cfg.DataDir = o.dataDir
	}
	return cfg, cfg.Validate()
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the icaro version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err
		},
	}
}
