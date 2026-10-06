package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"icaro/internal/apiclient"
	"icaro/internal/service"
)

// client builds an API client from config (server.url / server.token) and
// the ICARO_SERVER__TOKEN style environment overrides.
func (o *rootOpts) client() (*apiclient.Client, error) {
	cfg, err := o.loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Server.Token == "" {
		return nil, errors.New("no API token configured: set server.token in icaro.yaml or ICARO_SERVER__TOKEN")
	}
	return apiclient.New(cfg.Server.URL, cfg.Server.Token), nil
}

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
			c, err := opts.client()
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
			if info.WebhookToken != "" {
				cfg, _ := opts.loadConfig()
				fmt.Fprintf(cmd.OutOrStdout(), "webhook: POST %s/hooks/%s/%s\n", cfg.Server.URL, info.Name, info.WebhookToken)
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
			c, err := opts.client()
			if err != nil {
				return err
			}
			res, err := c.Validate(cmd.Context(), string(yaml))
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if res.Valid {
				fmt.Fprintf(cmd.OutOrStdout(), "valid: %s\n", res.Name)
				return nil
			}
			for _, is := range res.Issues {
				fmt.Fprintln(cmd.OutOrStdout(), is.String())
			}
			return fmt.Errorf("%d issue(s)", len(res.Issues))
		},
	}

	get := &cobra.Command{
		Use:   "get NAME",
		Short: "Show a workflow (YAML by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client()
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
			c, err := opts.client()
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
				var trig []string
				if w.WebhookToken != "" {
					trig = append(trig, "webhook")
				}
				for _, s := range w.Schedules {
					trig = append(trig, "cron("+s+")")
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n", w.Name, w.Version, w.Status, len(w.Steps), strings.Join(trig, ","), w.UpdatedAt)
			}
			return tw.Flush()
		},
	}

	del := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a workflow (runs are kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client()
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
				c, err := opts.client()
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
			c, err := opts.client()
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

func newRunCmd(opts *rootOpts) *cobra.Command {
	var inputPath, inputJSON string
	var wait, asJSON bool
	cmd := &cobra.Command{
		Use:   "run NAME",
		Short: "Start a workflow run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := map[string]any{}
			switch {
			case inputPath != "":
				b, err := readInputArg(inputPath)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(b, &input); err != nil {
					return fmt.Errorf("input file: %w", err)
				}
			case inputJSON != "":
				if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
					return fmt.Errorf("--input: %w", err)
				}
			}
			c, err := opts.client()
			if err != nil {
				return err
			}
			run, err := c.Run(cmd.Context(), args[0], input)
			if err != nil {
				return err
			}
			if wait {
				run, err = c.WaitRun(cmd.Context(), run.ID, time.Second)
				if err != nil {
					return err
				}
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), run)
			}
			printRun(cmd.OutOrStdout(), run)
			if wait && run.Status != "succeeded" {
				return fmt.Errorf("run %s", run.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&inputJSON, "input", "", "input payload as JSON")
	cmd.Flags().StringVar(&inputPath, "input-file", "", "input payload from a JSON file (- for stdin)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the run to finish")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printRun(w io.Writer, run *service.RunInfo) {
	fmt.Fprintf(w, "run %s  workflow=%s v%d  status=%s  trigger=%s\n", run.ID, run.Workflow, run.Version, run.Status, run.Trigger)
	if run.Error != "" {
		fmt.Fprintf(w, "error: %s\n", run.Error)
	}
	if len(run.Steps) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  STEP\tSTATUS\tATTEMPT\tEXIT\tOUTPUTS\tERROR")
	for _, st := range run.Steps {
		exit := "-"
		if st.ExitCode != nil {
			exit = strconv.Itoa(*st.ExitCode)
		}
		out := string(st.Outputs)
		if len(out) > 60 {
			out = out[:57] + "..."
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%s\t%s\n", st.Name, st.Status, st.Attempt, exit, out, st.Error)
	}
	_ = tw.Flush()
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
			c, err := opts.client()
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
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Workflow, r.Status, r.Trigger, r.CreatedAt, r.Error)
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
			c, err := opts.client()
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

	var tail int
	logs := &cobra.Command{
		Use:   "logs ID [STEP]",
		Short: "Print a step's log (step index, default 0)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			idx := 0
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("step must be an index")
				}
				idx = n
			}
			c, err := opts.client()
			if err != nil {
				return err
			}
			b, err := c.StepLogs(cmd.Context(), args[0], idx, tail)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	logs.Flags().IntVar(&tail, "tail", 0, "only the last N bytes")

	cancel := &cobra.Command{
		Use:   "cancel ID",
		Short: "Cancel a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client()
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

func newConnectionCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "connection", Short: "Manage connections (credentials)"}
	var typ string
	var fields []string
	create := &cobra.Command{
		Use:   "create NAME --type TYPE --field key=value [--field ...]",
		Short: "Create or replace a connection; values are encrypted at rest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kv := map[string]string{}
			for _, f := range fields {
				k, v, ok := strings.Cut(f, "=")
				if !ok {
					return fmt.Errorf("--field must be key=value, got %q", f)
				}
				if strings.HasPrefix(v, "@") {
					b, err := os.ReadFile(v[1:])
					if err != nil {
						return err
					}
					v = strings.TrimRight(string(b), "\n")
				}
				kv[k] = v
			}
			c, err := opts.client()
			if err != nil {
				return err
			}
			info, err := c.UpsertConnection(cmd.Context(), args[0], typ, kv)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "connection %s (%s) fields: %s\n", info.Name, info.Type, strings.Join(info.FieldNames, ", "))
			return nil
		},
	}
	create.Flags().StringVar(&typ, "type", "generic", "connection type: generic | bearer | basic")
	create.Flags().StringArrayVar(&fields, "field", nil, "field as key=value (value @file reads from a file)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List connections (names and types only)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := opts.client()
			if err != nil {
				return err
			}
			cs, err := c.ListConnections(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTYPE\tFIELDS\tUPDATED")
			for _, cn := range cs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cn.Name, cn.Type, strings.Join(cn.FieldNames, ","), cn.UpdatedAt)
			}
			return tw.Flush()
		},
	}
	del := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.client()
			if err != nil {
				return err
			}
			return c.DeleteConnection(cmd.Context(), args[0])
		},
	}
	cmd.AddCommand(create, list, del)
	return cmd
}

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the workflow JSON Schema",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(schemaJSON())
			return err
		},
	}
}
