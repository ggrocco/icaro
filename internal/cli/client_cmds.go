package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"icaro/internal/workflow"
)

func readInputArg(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func init() {
	register(newWorkflowCmd)
	register(newRunCmd)
	register(newRunsCmd)
	register(func(*rootOpts) *cobra.Command { return newSchemaCmd() })
}

func newWorkflowCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "workflow", Short: "Manage workflows"}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	apply := &cobra.Command{
		Use:   "apply FILE",
		Short: "Create or update a workflow from a YAML file (- for stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			yaml, err := readInputArg(args[0])
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			info, err := c.ApplyWorkflow(cmd.Context(), string(yaml))
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), info)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "applied %s version %d\n", info.Name, info.Version)
			if len(info.TriggerFields.Required) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "manual runs need: --set %s\n", strings.Join(info.TriggerFields.Required, "=... --set ")+"=...")
			}
			return nil
		},
	}

	validate := &cobra.Command{
		Use:   "validate FILE",
		Short: "Validate a workflow file without saving it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			yaml, err := readInputArg(args[0])
			if err != nil {
				return err
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.Validate(cmd.Context(), string(yaml))
			if err != nil {
				return err
			}
			switch {
			case asJSON:
				if err := printJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			case res.Valid:
				fmt.Fprintf(cmd.OutOrStdout(), "valid: %s\n", res.Name)
			default:
				for _, is := range res.Issues {
					fmt.Fprintln(cmd.OutOrStdout(), is.String())
				}
			}
			// Invalid fails in both output modes, so scripts can rely on it.
			if !res.Valid {
				return fmt.Errorf("%d issue(s)", len(res.Issues))
			}
			return nil
		},
	}

	get := &cobra.Command{
		Use:   "get NAME",
		Short: "Show a workflow (YAML by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			info, err := c.GetWorkflow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), info)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# %s v%d (%s)\n", info.Name, info.Version, info.Status)
			fmt.Fprint(cmd.OutOrStdout(), info.YAML)
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List workflows",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			ws, err := c.ListWorkflows(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), ws)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVERSION\tSTATUS\tSTEPS\tTRIGGERS\tUPDATED")
			for _, w := range ws {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n", w.Name, w.Version, w.Status, len(w.Steps), triggerSummary(w.Triggers), w.UpdatedAt)
			}
			return tw.Flush()
		},
	}

	del := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a workflow (runs are kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			return c.DeleteWorkflow(cmd.Context(), args[0])
		},
	}

	toggle := func(use string, enabled bool) *cobra.Command {
		return &cobra.Command{
			Use:   use + " NAME",
			Short: strings.ToUpper(use[:1]) + use[1:] + " a workflow",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := opts.client(cmd.Context())
				if err != nil {
					return err
				}
				info, err := c.SetWorkflowEnabled(cmd.Context(), args[0], enabled)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s is %s\n", info.Name, info.Status)
				return nil
			},
		}
	}

	versions := &cobra.Command{
		Use:   "versions NAME [VERSION]",
		Short: "List versions, or print one version's YAML",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("version must be an integer")
				}
				v, err := c.GetVersion(cmd.Context(), args[0], n)
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), v.YAML)
				return nil
			}
			vs, err := c.ListVersions(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), vs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tACTOR\tCREATED")
			for _, v := range vs {
				fmt.Fprintf(tw, "%d\t%s\t%s\n", v.Version, v.Actor, v.CreatedAt)
			}
			return tw.Flush()
		},
	}

	cmd.AddCommand(apply, validate, get, list, del, toggle("enable", true), toggle("disable", false), versions)
	return cmd
}

func newRunsCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "runs", Short: "Inspect runs"}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	var wfName, status string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(cmd.Context(), wfName, status, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), runs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tWORKFLOW\tSTATUS\tTRIGGER\tCREATED\tERROR")
			for _, r := range runs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Workflow, r.Status, r.TriggerKind, r.CreatedAt, r.Error)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&wfName, "workflow", "", "filter by workflow")
	list.Flags().StringVar(&status, "status", "", "filter by status")
	list.Flags().IntVar(&limit, "limit", 50, "max rows")

	get := &cobra.Command{
		Use:   "get ID",
		Short: "Show a run and its steps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			run, err := c.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), run)
			}
			printRun(cmd.OutOrStdout(), run)
			return nil
		},
	}

	var tail, attempt int
	var follow bool
	logs := &cobra.Command{
		Use:   "logs ID [STEP]",
		Short: "Print a step's log (default: every step, in plan order); --follow streams",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			step := ""
			if len(args) == 2 {
				step = args[1]
			}
			if follow {
				_, err := followRun(cmd.Context(), c, args[0], step, cmd.OutOrStdout())
				return err
			}
			steps := []string{step}
			if step == "" {
				run, err := c.GetRun(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				steps = steps[:0]
				for _, st := range run.Steps {
					if st.Attempt > 0 {
						steps = append(steps, st.ID)
					}
				}
			}
			for _, id := range steps {
				b, err := c.StepLogs(cmd.Context(), args[0], id, attempt, tail)
				if err != nil {
					return err
				}
				if len(steps) > 1 || step == "" {
					writePrefixed(cmd.OutOrStdout(), id, b)
					continue
				}
				if _, err := cmd.OutOrStdout().Write(b); err != nil {
					return err
				}
			}
			return nil
		},
	}
	logs.Flags().IntVar(&tail, "tail", 0, "only the last N bytes")
	logs.Flags().IntVar(&attempt, "attempt", 0, "attempt number (default: latest)")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "stream until the run ends")

	cancel := &cobra.Command{
		Use:   "cancel ID",
		Short: "Cancel a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			run, err := c.CancelRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %s is %s\n", run.ID, run.Status)
			return nil
		},
	}
	cmd.AddCommand(list, get, logs, cancel)
	return cmd
}

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the workflow JSON Schema",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(workflow.SchemaJSON())
			return err
		},
	}
}
