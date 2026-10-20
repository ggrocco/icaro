package cli

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"icaro/internal/update"
)

func init() { register(newUpdateCmd) }

func newUpdateCmd(opts *rootOpts) *cobra.Command {
	var check, rollback, yes, noRestart bool
	var target string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update icaro to the latest signed release (or --rollback)",
		Long: `Downloads the release archive, checksums.txt and its ed25519 signature,
verifies both, smoke-tests the new binary, swaps it in atomically and keeps
the previous binary for --rollback. Config and the database under ~/.icaro are
never touched; a running daemon is restarted and re-attaches its sessions.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			exe, err := update.Executable()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			restart := func() error {
				if noRestart {
					return nil
				}
				restarted, err := update.RestartDaemon(cmd.Context(), cfg.Layout(), exe, opts.daemonArgs())
				if err != nil {
					return fmt.Errorf("binary updated, but restarting the daemon failed: %w", err)
				}
				if restarted {
					fmt.Fprintln(out, "daemon restarted")
				}
				return nil
			}
			if rollback {
				if !yes {
					return errors.New("rollback replaces the installed binary; re-run with --yes")
				}
				if err := update.Rollback(exe); err != nil {
					return err
				}
				fmt.Fprintf(out, "restored %s from %s\n", exe, update.PrevPath(exe))
				return restart()
			}

			hc := &http.Client{Timeout: 5 * time.Minute}
			u := &update.Updater{Exe: exe, Current: version, Source: update.NewSource(cfg.Update.BaseURL, hc), HTTP: hc}
			plan, err := u.Check(cmd.Context())
			if target != "" {
				plan = update.Plan{Current: version, Latest: target, Available: true, Release: u.Source.At(target)}
				err = nil
			}
			if err != nil {
				return err
			}
			if !plan.Available {
				fmt.Fprintf(out, "icaro %s is up to date (latest %s)\n", plan.Current, plan.Latest)
				return nil
			}
			fmt.Fprintf(out, "update available: %s -> %s\n", plan.Current, plan.Latest)
			if check {
				return nil
			}
			if !yes {
				return errors.New("re-run with --yes to install it")
			}
			key := cfg.Update.PublicKey
			if key == "" {
				key = update.DefaultPublicKey
			}
			pub, err := update.ParsePublicKey(key)
			if err != nil {
				return fmt.Errorf("cannot verify releases: %w (set update.public_key)", err)
			}
			u.PublicKey = pub
			if err := u.Apply(cmd.Context(), plan.Release); err != nil {
				return err
			}
			fmt.Fprintf(out, "installed %s at %s (previous kept as %s)\n", plan.Latest, exe, update.PrevPath(exe))
			return restart()
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether an update is available")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "restore the binary kept by the last update")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm replacing the installed binary")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart a running daemon")
	cmd.Flags().StringVar(&target, "version", "", "install this version instead of the latest (e.g. v0.3.0)")
	return cmd
}
