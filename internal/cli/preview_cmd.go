package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"icaro/internal/apiclient"
	"icaro/internal/app"
	"icaro/internal/preview"
)

func init() {
	register(newPreviewCmd)
	extraDoctorChecks = append(extraDoctorChecks, app.PreviewChecks)
}

func printPreviewJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// previewPath resolves an optional PATH argument (default: the current
// directory) to an absolute path: the daemon has its own working dir.
func previewPath(args []string) (string, error) {
	p := "."
	if len(args) > 0 {
		p = args[0]
	}
	return filepath.Abs(p)
}

func printPreview(w io.Writer, p *apiclient.Preview) {
	fmt.Fprintf(w, "%s  %s\n", p.Status, p.WorktreePath)
	if p.URL != "" {
		fmt.Fprintf(w, "  url:  %s\n", p.URL)
		fmt.Fprintf(w, "  curl: %s\n", p.Curl)
	}
	if p.Service != "" {
		fmt.Fprintf(w, "  service %s port %d, compose project %s\n", p.Service, p.Port, p.ComposeProject)
	}
	if p.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", p.Error)
	}
}

func newPreviewCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "preview", Short: "Per-worktree previews: compose projects routed at a predictable URL"}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	list := &cobra.Command{
		Use:   "list",
		Short: "List previews",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			ps, err := c.ListPreviews(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printPreviewJSON(cmd.OutOrStdout(), ps)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tURL\tWORKTREE")
			for _, p := range ps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.ID, p.Status, p.URL, p.WorktreePath)
			}
			return tw.Flush()
		},
	}

	var force bool
	up := &cobra.Command{
		Use:   "up [PATH]",
		Short: "Bring up the preview of a worktree (default: current directory)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := previewPath(args)
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			p, err := c.PreviewUp(cmd.Context(), path, force)
			if err != nil {
				return err
			}
			if asJSON {
				return printPreviewJSON(cmd.OutOrStdout(), p)
			}
			printPreview(cmd.OutOrStdout(), p)
			if p.Status != "running" {
				return fmt.Errorf("preview is %s", p.Status)
			}
			return nil
		},
	}
	up.Flags().BoolVar(&force, "force", false, "restart even if the configuration did not change")

	down := &cobra.Command{
		Use:   "down [PATH]",
		Short: "Stop a preview and keep its record",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := previewPath(args)
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			p, err := c.PreviewDown(cmd.Context(), path)
			if err != nil {
				return err
			}
			if asJSON {
				return printPreviewJSON(cmd.OutOrStdout(), p)
			}
			printPreview(cmd.OutOrStdout(), p)
			return nil
		},
	}

	var yes bool
	rm := &cobra.Command{
		Use:   "rm [PATH]",
		Short: "Tear a preview down and remove its containers, volumes and record",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("removing a preview deletes its compose volumes; pass --yes to confirm")
			}
			path, err := previewPath(args)
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			return c.PreviewRemove(cmd.Context(), path)
		},
	}
	rm.Flags().BoolVar(&yes, "yes", false, "confirm the removal")

	var tail int
	logs := &cobra.Command{
		Use:   "logs ID|PATH",
		Short: "Show the compose logs of a preview",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			if ref == "" || filepath.IsAbs(ref) || ref[0] == '.' || containsSep(ref) {
				path, err := previewPath(args)
				if err != nil {
					return err
				}
				ps, err := c.ListPreviews(cmd.Context())
				if err != nil {
					return err
				}
				ref = ""
				for _, p := range ps {
					if p.WorktreePath == path {
						ref = p.ID
					}
				}
				if ref == "" {
					return fmt.Errorf("no preview for %s", path)
				}
			}
			b, err := c.PreviewLogs(cmd.Context(), ref, tail)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	logs.Flags().IntVar(&tail, "tail", 200, "lines per container")

	lint := &cobra.Command{
		Use:   "lint [PATH]",
		Short: "Check a worktree's compose file against the preview policy (works without the daemon)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := previewPath(args)
			if err != nil {
				return err
			}
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			st, err := openStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			m := app.NewPreviewManager(cfg, st, nil, nil)
			res, err := m.Lint(cmd.Context(), path)
			if err != nil {
				return err
			}
			if asJSON {
				if err := printPreviewJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else {
				printLint(cmd.OutOrStdout(), res)
			}
			if len(res.Blocked) > 0 {
				return fmt.Errorf("%d blocked finding(s)", len(res.Blocked))
			}
			return nil
		},
	}

	var findings []string
	var yesAllow bool
	allow := &cobra.Command{
		Use:   "allow PATH --finding ID [--finding ID...] --yes",
		Short: "Explicitly allow compose-lint findings for one worktree",
		Long:  "Allowing a finding weakens the preview's isolation from your machine (e.g. a privileged container or a host mount). The override is recorded per worktree and finding.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(findings) == 0 {
				return errors.New("name at least one --finding (see icaro preview lint)")
			}
			if !yesAllow {
				return errors.New("allowing a finding weakens the preview sandbox; pass --yes to confirm")
			}
			path, err := previewPath(args)
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.PreviewAllow(cmd.Context(), path, findings)
			if err != nil {
				return err
			}
			if asJSON {
				return printPreviewJSON(cmd.OutOrStdout(), res)
			}
			printLint(cmd.OutOrStdout(), res)
			return nil
		},
	}
	allow.Flags().StringArrayVar(&findings, "finding", nil, "finding ID to allow (repeatable)")
	allow.Flags().BoolVar(&yesAllow, "yes", false, "confirm the override")

	cmd.AddCommand(list, up, down, rm, logs, lint, allow)
	return cmd
}

func printLint(w io.Writer, res *preview.LintResult) {
	if len(res.Findings) == 0 {
		fmt.Fprintf(w, "ok: %s passes the preview policy\n", res.ComposeFile)
		return
	}
	blocked := map[string]bool{}
	for _, f := range res.Blocked {
		blocked[f.ID] = true
	}
	for _, f := range res.Findings {
		state := "allowed"
		if blocked[f.ID] {
			state = "BLOCKED"
		}
		fmt.Fprintf(w, "[%s] %s\n", state, f)
	}
}

func containsSep(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == os.PathSeparator {
			return true
		}
	}
	return false
}
