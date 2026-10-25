package cli

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"icaro/internal/autostart"
	"icaro/internal/config"
	"icaro/internal/update"
)

func init() { register(newServiceCmd) }

// serviceManager describes the login service for this home.
func serviceManager(opts *rootOpts, cfg *config.Config, exe string) (*autostart.Manager, error) {
	userDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &autostart.Manager{
		GOOS: runtime.GOOS, UserDir: userDir, UID: os.Getuid(), Exe: exe, Args: opts.daemonArgs(),
		Home: cfg.Home, LogFile: cfg.Layout().DaemonLog(), Run: autostart.ExecRunner,
	}, nil
}

func newServiceCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Start the daemon at login (launchd on macOS, systemd --user on Linux)",
	}
	manager := func() (*autostart.Manager, error) {
		cfg, err := opts.loadConfig()
		if err != nil {
			return nil, err
		}
		exe, err := update.Executable()
		if err != nil {
			return nil, err
		}
		return serviceManager(opts, cfg, exe)
	}
	var dryRun bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install and start the login service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			if dryRun {
				u, err := m.Unit()
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s", u.Path, u.Content)
				return nil
			}
			u, err := m.Install(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed %s; the daemon starts at login without opening a window\n", u.Path)
			return nil
		},
	}
	install.Flags().BoolVar(&dryRun, "dry-run", false, "print the unit instead of installing it")
	var yes bool
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the login service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return fmt.Errorf("this stops the daemon and removes the login service; re-run with --yes")
			}
			m, err := manager()
			if err != nil {
				return err
			}
			if err := m.Uninstall(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "login service removed")
			return nil
		},
	}
	uninstall.Flags().BoolVar(&yes, "yes", false, "confirm")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the login service is installed and running",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			st, err := m.Status(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unit: %s\ninstalled: %v\nactive: %v\n", st.Path, st.Installed, st.Active)
			return nil
		},
	}
	cmd.AddCommand(install, uninstall, status)
	return cmd
}
