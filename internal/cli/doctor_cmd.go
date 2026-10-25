package cli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/spf13/cobra"

	"icaro/internal/app"
	"icaro/internal/container/docker"
	"icaro/internal/doctor"
)

func init() { register(newDoctorCmd) }

// doctorChecks builds the check list without a daemon, so doctor works
// when the daemon is down (its most important case).
var doctorChecks = func(cmd *cobra.Command, opts *rootOpts) ([]doctor.Check, func(), error) {
	cfg, err := opts.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	var cleanup []func()
	st, err := openStore(cmd.Context(), cfg)
	if err != nil {
		return nil, nil, err
	}
	cleanup = append(cleanup, func() { _ = st.Close() })
	// eng stays a nil *docker.Engine for the component factories (which
	// test it directly), but BaseChecks takes an interface: hand it a nil
	// interface, not a typed nil, plus the reason the client failed.
	var eng *docker.Engine
	var pinger interface{ Ping(context.Context) error }
	e, engErr := docker.New(cfg.Docker.Host)
	if engErr == nil {
		eng, pinger = e, e
		cleanup = append(cleanup, func() { _ = e.Close() })
	}
	checks := app.BaseChecks(cfg, st, pinger, engErr)
	for _, extra := range extraDoctorChecks {
		checks = append(checks, extra(cmd.Context(), cfg, st, eng)...)
	}
	return checks, func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}, nil
}

// extraDoctorChecks lets other command files contribute checks.
var extraDoctorChecks []app.CheckFactory

func newDoctorCmd(opts *rootOpts) *cobra.Command {
	var fix, asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation and repair orphaned state (--fix)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks, cleanup, err := doctorChecks(cmd, opts)
			if err != nil {
				return err
			}
			defer cleanup()
			reports := doctor.Run(cmd.Context(), checks, fix)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(reports); err != nil {
					return err
				}
			} else {
				doctor.Print(cmd.OutOrStdout(), reports)
			}
			if doctor.Worst(reports) == doctor.Error {
				return errors.New("doctor found errors")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "apply remediations after validation")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
