package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"icaro/internal/hosts"
)

func init() { register(newHostCmd) }

func newHostCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Sign the daemon in to remote Ícaro daemons and list them",
		Long: "The daemon can be signed in to any number of remote Ícaro daemons at once. Each\n" +
			"has an alias (the short name references use, as in prod:deploy) and a label (its\n" +
			"display name). This machine is always the host \"local\" and needs no sign-in.\n" +
			"`icaro workflow list --all-hosts` lists the workflows of every host at once.",
	}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	var label string
	login := &cobra.Command{
		Use:   "login ALIAS URL [--label LABEL]",
		Short: "Sign in to a remote daemon with one of its API tokens (read from stdin or a hidden prompt)",
		Long: "Sign in to the Ícaro daemon at URL with an API token created there\n" +
			"(`icaro token create --name NAME --scope read|write|admin`). The token is read from\n" +
			"stdin or a hidden prompt, never from argv, checked against the remote daemon, sealed\n" +
			"with the master key and only ever sent to URL. Signing in again with the same\n" +
			"alias replaces it.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := readSecret("API token for "+args[1]+": ", cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("empty token")
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			h, err := c.LoginHost(cmd.Context(), hosts.Login{Alias: args[0], Label: label, URL: args[1], Token: token})
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), h)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed in to %s (%s) at %s as %s, scope %s\n", h.Alias, h.Label, h.URL, h.Principal, h.Scope)
			return nil
		},
	}
	login.Flags().StringVar(&label, "label", "", "display name of the host (default: the alias)")

	logout := &cobra.Command{
		Use:   "logout ALIAS",
		Short: "Forget the token of a remote daemon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.LogoutHost(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed out of %s\n", args[0])
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List this machine and every signed-in host",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			hs, err := c.ListHosts(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), hs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ALIAS\tLABEL\tURL\tSIGNED IN AS\tSCOPE")
			for _, h := range hs {
				url, who, scope := h.URL, h.Principal, h.Scope
				if h.Local {
					url, who, scope = "(this machine)", "-", "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Alias, h.Label, url, who, scope)
			}
			return tw.Flush()
		},
	}

	cmd.AddCommand(login, logout, list)
	return cmd
}

// printHostWorkflows prints a listing of every host's workflows, then the
// hosts that could not be listed. It fails when a host failed, so scripts
// notice a partial listing.
func printHostWorkflows(w io.Writer, l *hosts.Listing) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REF\tHOST\tVERSION\tSTATUS\tSTEPS\tTRIGGERS\tUPDATED")
	for _, wf := range l.Workflows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\t%s\t%s\n", wf.Ref, wf.HostLabel, wf.Version, wf.Status, len(wf.Steps), triggerSummary(wf.Triggers), wf.UpdatedAt)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	failed := 0
	for _, h := range l.Hosts {
		if h.Error != "" {
			fmt.Fprintf(w, "%s (%s): %s\n", h.Alias, h.Label, h.Error)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d host(s) could not be listed", failed, len(l.Hosts))
	}
	return nil
}
