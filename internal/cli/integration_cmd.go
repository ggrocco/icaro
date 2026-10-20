package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"icaro/internal/config"
	"icaro/internal/container/docker"
	"icaro/internal/daemon"
	"icaro/internal/doctor"
	"icaro/internal/integration"
	"icaro/internal/store"
)

func init() {
	register(newIntegrationCmd)
	register(newCredentialCmd)
	register(newDeliveriesCmd)
	register(newSchedulesCmd)
	extraDoctorChecks = append(extraDoctorChecks, func(_ context.Context, cfg *config.Config, st *store.Store, _ *docker.Engine) []doctor.Check {
		return integration.DoctorChecks(cfg.Layout().Integrations(), st)
	})
}

func intgJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// confirmPrompt asks a yes/no question on the command's stdin.
func intgConfirm(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// reloadIfRunning nudges a running daemon after a pack change; it never
// starts one (the watcher would pick the change up anyway).
func (o *rootOpts) reloadIfRunning(ctx context.Context, cfg *config.Config) {
	if cfg.Daemon.URL == "" && daemon.Ping(ctx, cfg.Layout()) != nil {
		return
	}
	if c, err := o.client(ctx); err == nil {
		_, _ = c.ReloadIntegrations(ctx)
	}
}

func newIntegrationCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "integration", Aliases: []string{"integrations"}, Short: "Manage integration manifests and packs (ADR 0007)"}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	list := &cobra.Command{
		Use:   "list",
		Short: "List loaded integrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			ins, err := c.Integrations(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return intgJSON(cmd.OutOrStdout(), ins)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPACK\tEVENTS\tACTIONS\tHOSTS")
			for _, in := range ins {
				pack := in.Pack
				if in.Shadows {
					pack += " (shadows builtin)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", in.Name, pack, len(in.Events), len(in.Actions), strings.Join(in.Hosts, ","))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			st, err := c.IntegrationStatus(cmd.Context())
			if err == nil {
				printIntegrationProblems(cmd.OutOrStdout(), st)
			}
			return nil
		},
	}

	var showManifest bool
	show := &cobra.Command{
		Use:   "show NAME",
		Short: "Show an integration's permissions, events and actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			in, err := c.Integration(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return intgJSON(cmd.OutOrStdout(), in)
			}
			if showManifest {
				_, err := io.WriteString(cmd.OutOrStdout(), in.Manifest)
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s (pack %s, %s)\n%s\n", in.Name, in.Pack, in.File, in.Description)
			fmt.Fprintf(w, "hosts:  %s\nauth:   %s\nverify: %s\n", strings.Join(in.Hosts, ", "), strings.Join(in.Auth, ", "), in.Verify)
			for _, e := range in.Events {
				ev := in.EventsFull[e]
				fmt.Fprintf(w, "event  %s: trigger.{%s}\n", e, strings.Join(ev.Fields, ", "))
			}
			for _, a := range in.Actions {
				ai := in.ActionsFull[a]
				var inputs []string
				for k, v := range ai.Inputs {
					s := k + ":" + v.Type
					if v.Required {
						s += "!"
					}
					inputs = append(inputs, s)
				}
				fmt.Fprintf(w, "action %s.%s(%s) -> %s\n", in.Name, a, strings.Join(intgSorted(inputs), ", "), strings.Join(ai.Outputs, ", "))
			}
			return nil
		},
	}
	show.Flags().BoolVar(&showManifest, "manifest", false, "print the manifest YAML")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show packs, load errors and missing webhook secrets",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			st, err := c.IntegrationStatus(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return intgJSON(cmd.OutOrStdout(), st)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PACK\tHEAD\tREMOTE\tINTEGRATIONS")
			for _, p := range st.Packs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, intgDash(p.Head), intgDash(p.Remote), strings.Join(p.Integrations, ","))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			printIntegrationProblems(cmd.OutOrStdout(), st)
			return nil
		},
	}

	var packName string
	var yes bool
	add := &cobra.Command{
		Use:   "add GIT-URL",
		Short: "Install a third-party pack after showing its complete permission summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.Layout().Ensure(); err != nil {
				return err
			}
			packs := integration.NewPacks(cfg.Layout().Integrations(), nil)
			staged, err := packs.Stage(cmd.Context(), args[0], packName)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), staged.Summary.Format())
			if len(staged.Summary.Integrations) == 0 {
				_ = packs.Discard(staged)
				return errors.New("the pack has no valid manifests")
			}
			if !yes && !intgConfirm(cmd, "Activate this pack?") {
				_ = packs.Discard(staged)
				return errors.New("not activated")
			}
			if err := packs.Activate(staged); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed pack %s\n", staged.Name)
			opts.reloadIfRunning(cmd.Context(), cfg)
			return nil
		},
	}
	add.Flags().StringVar(&packName, "name", "", "pack name (default: derived from the URL)")
	add.Flags().BoolVar(&yes, "yes", false, "activate without asking")

	update := &cobra.Command{
		Use:   "update PACK",
		Short: "Pull a pack's latest commits and show its new permission summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			sum, err := integration.NewPacks(cfg.Layout().Integrations(), nil).Update(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), sum.Format())
			opts.reloadIfRunning(cmd.Context(), cfg)
			return nil
		},
	}

	push := &cobra.Command{
		Use:   "push PACK",
		Short: "Push a pack's commits (e.g. UI edits to the local pack) to its remote",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			return integration.NewPacks(cfg.Layout().Integrations(), nil).Push(cmd.Context(), args[0])
		},
	}

	var removeYes bool
	remove := &cobra.Command{
		Use:   "remove PACK",
		Short: "Delete an installed pack (requires --yes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !removeYes {
				return errors.New("removing a pack deletes its directory; pass --yes to confirm")
			}
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			if err := integration.NewPacks(cfg.Layout().Integrations(), nil).Remove(args[0]); err != nil {
				return err
			}
			opts.reloadIfRunning(cmd.Context(), cfg)
			return nil
		},
	}
	remove.Flags().BoolVar(&removeYes, "yes", false, "confirm deletion")

	var editFile string
	edit := &cobra.Command{
		Use:   "edit NAME -f FILE",
		Short: "Validate a manifest and commit it to the local pack (- reads stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var src []byte
			var err error
			if editFile == "-" {
				src, err = io.ReadAll(cmd.InOrStdin())
			} else {
				src, err = os.ReadFile(editFile)
			}
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.EditManifest(cmd.Context(), args[0], string(src))
			if err != nil {
				return err
			}
			if !res.Changed {
				fmt.Fprintln(cmd.OutOrStdout(), "unchanged")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "committed %s to pack %s (push with `icaro integration push %s`)\n", intgShort(res.Commit), res.Pack, res.Pack)
			return nil
		},
	}
	edit.Flags().StringVarP(&editFile, "file", "f", "", "manifest file")
	_ = edit.MarkFlagRequired("file")

	var rotate string
	tokens := &cobra.Command{
		Use:   "webhook-tokens",
		Short: "List the generic webhook URL tokens (one per subscribed workflow)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			if rotate != "" {
				t, err := c.RotateWebhookToken(cmd.Context(), rotate)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tPOST %s\n", t.Workflow, intgFirst(t.URL, t.Path))
				return nil
			}
			toks, err := c.WebhookTokens(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return intgJSON(cmd.OutOrStdout(), toks)
			}
			for _, t := range toks {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tPOST %s\n", t.Workflow, intgFirst(t.URL, t.Path))
			}
			return nil
		},
	}
	tokens.Flags().StringVar(&rotate, "rotate", "", "issue a new token for this workflow")

	schema := &cobra.Command{
		Use:   "schema",
		Short: "Print the integration manifest JSON Schema",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(integration.SchemaJSON())
			return err
		},
	}

	validate := &cobra.Command{
		Use:   "validate FILE",
		Short: "Validate a manifest file offline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			m, issues, err := integration.Parse(src)
			if err != nil {
				return err
			}
			if len(issues) > 0 {
				for _, is := range issues {
					fmt.Fprintln(cmd.OutOrStdout(), is.String())
				}
				return fmt.Errorf("%d issue(s)", len(issues))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "valid: %s\n", m.Name)
			return nil
		},
	}

	cmd.AddCommand(list, show, status, add, update, push, remove, edit, tokens, schema, validate)
	return cmd
}

func printIntegrationProblems(w io.Writer, st *integration.Status) {
	for _, wr := range st.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wr)
	}
	for _, e := range st.Errors {
		suffix := ""
		if e.ServingLastGood {
			suffix = " (serving the last good version)"
		}
		fmt.Fprintf(w, "error: %s/%s: %s%s\n", e.Pack, e.File, e.Error, suffix)
	}
	for _, m := range st.MissingSecrets {
		integ, cred, _ := strings.Cut(m, ".")
		fmt.Fprintf(w, "error: missing webhook secret %s (icaro credential add %s %s)\n", m, integ, cred)
	}
}

func newCredentialCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "credential", Aliases: []string{"credentials"}, Short: "Manage host-bound integration credentials"}

	var hosts []string
	var yes bool
	add := &cobra.Command{
		Use:   "add INTEGRATION [NAME]",
		Short: "Store a credential (value read from stdin), bound to the hosts it may be sent to",
		Long: "Store `<integration>.<name>` in the daemon's secret store. The value is read from stdin.\n" +
			"Action credentials are bound to the manifest's hosts (plus a configured base_url host)\n" +
			"unless --host is given; the executor refuses to send them anywhere else. Webhook\n" +
			"secrets (webhook_secret) are only used to verify inbound requests and get no binding.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "token"
			if len(args) == 2 {
				name = args[1]
			}
			ctx := cmd.Context()
			c, err := opts.client(ctx)
			if err != nil {
				return err
			}
			plan, err := c.PlanCredential(ctx, args[0], name)
			if err != nil {
				return err
			}
			if len(hosts) > 0 && !plan.Webhook {
				plan.Hosts = hosts
			}
			w := cmd.OutOrStdout()
			if plan.Webhook {
				fmt.Fprintf(w, "%s verifies inbound webhooks and is never sent anywhere.\n", plan.Secret)
			} else {
				fmt.Fprintf(w, "%s will only be sent to: %s\n", plan.Secret, strings.Join(plan.Hosts, ", "))
			}
			in := bufio.NewReader(cmd.InOrStdin())
			if !yes {
				fmt.Fprint(w, "Continue? [y/N] ")
				line, _ := in.ReadString('\n')
				if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" {
					return errors.New("not stored")
				}
			}
			fmt.Fprintf(w, "value for %s (input is read from stdin): ", plan.Secret)
			value, err := in.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			value = strings.TrimRight(value, "\r\n")
			if value == "" {
				return errors.New("empty value")
			}
			var bind []string
			if !plan.Webhook {
				bind = plan.Hosts
			}
			stored, err := c.AddCredential(ctx, args[0], name, value, bind)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "\nstored %s\n", stored.Secret)
			return nil
		},
	}
	add.Flags().StringSliceVar(&hosts, "host", nil, "bind to these hosts instead of the manifest's (repeatable; *.domain allowed)")
	add.Flags().BoolVar(&yes, "yes", false, "store without asking to confirm the binding")

	list := &cobra.Command{
		Use:   "list",
		Short: "List integration credentials (names and host bindings, never values)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			creds, err := c.Credentials(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tHOSTS\tUPDATED")
			for _, cr := range creds {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", cr.Name, intgDash(strings.Join(cr.Hosts, ",")), cr.UpdatedAt)
			}
			return tw.Flush()
		},
	}

	var rmYes bool
	remove := &cobra.Command{
		Use:   "remove NAME",
		Short: "Delete a credential (requires --yes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !rmYes {
				return errors.New("pass --yes to delete the credential")
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			return c.DeleteCredential(cmd.Context(), args[0])
		},
	}
	remove.Flags().BoolVar(&rmYes, "yes", false, "confirm deletion")
	cmd.AddCommand(add, list, remove)
	return cmd
}

func newDeliveriesCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "deliveries", Aliases: []string{"delivery"}, Short: "Inspect and redeliver webhook deliveries"}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")
	var integ, status string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List recent deliveries",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			ds, err := c.Deliveries(cmd.Context(), integ, status, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return intgJSON(cmd.OutOrStdout(), ds)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tINTEGRATION\tSTATUS\tRUNS\tRECEIVED\tDETAIL")
			for _, d := range ds {
				runs := 0
				for _, r := range d.Results {
					if r.RunID != "" {
						runs++
					}
				}
				detail := intgFirst(d.VerifyError, d.Error)
				if d.RedeliveryOf != "" {
					detail = strings.TrimSpace("redelivery of " + d.RedeliveryOf + " " + detail)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", d.ID, d.Integration, d.Status, runs, d.ReceivedAt, detail)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&integ, "integration", "", "only this integration")
	list.Flags().StringVar(&status, "status", "", "only this status (rejected, duplicate, no_match, processed, challenge, error)")
	list.Flags().IntVar(&limit, "limit", 50, "maximum rows")

	get := &cobra.Command{
		Use:   "get ID",
		Short: "Show a delivery: headers, body, verification, matched workflows and runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			d, err := c.Delivery(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return intgJSON(cmd.OutOrStdout(), d)
		},
	}

	redeliver := &cobra.Command{
		Use:   "redeliver ID",
		Short: "Re-run the pipeline from a recorded, verified delivery",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			out, err := c.Redeliver(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return intgJSON(cmd.OutOrStdout(), out)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "delivery %s: %s\n", out.DeliveryID, out.Status)
			for _, r := range out.Results {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s (%s): %s\n", r.Workflow, r.Event, intgFirst(r.RunID, r.Skipped, r.Error))
			}
			return nil
		},
	}
	cmd.AddCommand(list, get, redeliver)
	return cmd
}

func newSchedulesCmd(opts *rootOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "schedules",
		Short: "List cron triggers with their persisted next fire times",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			ss, err := c.Schedules(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "WORKFLOW\tTRIGGER\tCRON\tNEXT\tLAST FIRED")
			for _, s := range ss {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", s.Workflow, s.Trigger, s.Cron, s.NextAt, intgDash(s.LastFiredAt))
			}
			return tw.Flush()
		},
	}
}

func intgDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func intgShort(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func intgFirst(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func intgSorted(s []string) []string {
	sort.Strings(s)
	return s
}
