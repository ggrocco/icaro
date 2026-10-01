package cli

import (
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

func newConfigCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration as YAML (token redacted)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			if cfg.Server.Token != "" {
				cfg.Server.Token = "***"
			}
			return yaml.NewEncoder(cmd.OutOrStdout()).Encode(cfg)
		},
	})
	return cmd
}
