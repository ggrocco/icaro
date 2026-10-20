package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func init() { register(newSecretCmd) }

func newSecretCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Manage secrets for ${{ secrets.* }} and integration credentials"}
	var hosts []string
	set := &cobra.Command{
		Use:   "set NAME [--host api.example.com ...]",
		Short: "Store a secret; the value is read from stdin or a hidden prompt, never from argv",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := readSecret("Value for "+args[0]+": ", cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if value == "" {
				return errors.New("empty value")
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.SetSecret(cmd.Context(), args[0], value, hosts); err != nil {
				return err
			}
			bound := "not bound to any host (container env only)"
			if len(hosts) > 0 {
				bound = "bound to " + strings.Join(hosts, ", ")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored %s, %s\n", args[0], bound)
			return nil
		},
	}
	set.Flags().StringSliceVar(&hosts, "host", nil, "host the value may be sent to (repeatable; *.domain allowed)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List secret names and host bindings (values are never shown)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			infos, err := c.ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tHOSTS\tUPDATED")
			for _, in := range infos {
				h := strings.Join(in.Hosts, ",")
				if h == "" {
					h = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", in.Name, h, in.UpdatedAt.Local().Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}

	var yes bool
	rm := &cobra.Command{
		Use:   "rm NAME",
		Short: "Delete a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				ok, err := confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), "Delete secret "+args[0]+"?")
				if err != nil || !ok {
					return err
				}
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			return c.DeleteSecret(cmd.Context(), args[0])
		},
	}
	rm.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.AddCommand(set, list, rm)
	return cmd
}
