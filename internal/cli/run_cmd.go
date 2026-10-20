package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"icaro/internal/apiclient"
	"icaro/internal/service"
	"icaro/internal/workflow"
)

func newRunCmd(opts *rootOpts) *cobra.Command {
	var sets []string
	var triggerJSON, triggerFile string
	var wait, asJSON, noPrompt bool
	cmd := &cobra.Command{
		Use:   "run NAME",
		Short: "Start a workflow run manually",
		Long: `Start a workflow run manually. The run's ${{ trigger.* }} payload comes
from --trigger-json/--trigger-file and --set key=value; any field the
workflow requires that is still missing is prompted for on a terminal.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			trigger := map[string]any{}
			switch {
			case triggerFile != "":
				b, err := readInputArg(triggerFile)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(b, &trigger); err != nil {
					return fmt.Errorf("--trigger-file: %w", err)
				}
			case triggerJSON != "":
				if err := json.Unmarshal([]byte(triggerJSON), &trigger); err != nil {
					return fmt.Errorf("--trigger-json: %w", err)
				}
			}
			for _, kv := range sets {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return fmt.Errorf("--set %q: want key=value", kv)
				}
				trigger[k] = parseValue(v)
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			info, err := c.GetWorkflow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var missing []string
			for _, f := range info.TriggerFields.Required {
				if _, ok := trigger[f]; !ok {
					missing = append(missing, f)
				}
			}
			if len(missing) > 0 {
				if noPrompt || !isTerminal(os.Stdin) {
					return fmt.Errorf("workflow %s needs trigger field(s) %s (pass --set %s=<value>)", info.Name, strings.Join(missing, ", "), missing[0])
				}
				if err := prompt(cmd.InOrStdin(), cmd.ErrOrStderr(), missing, trigger); err != nil {
					return err
				}
			}
			run, err := c.Run(cmd.Context(), args[0], trigger)
			if err != nil {
				return err
			}
			if !wait {
				if asJSON {
					return printJSON(cmd.OutOrStdout(), run)
				}
				printRun(cmd.OutOrStdout(), run)
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "run %s queued; streaming logs\n", run.ID)
			out := cmd.OutOrStdout()
			if asJSON {
				out = io.Discard
			}
			final, err := followRun(cmd.Context(), c, run.ID, "", out)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), final)
			}
			printRun(cmd.ErrOrStderr(), final)
			if final.Status != "succeeded" {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&sets, "set", nil, "trigger field key=value (repeatable; numbers/booleans/JSON are parsed)")
	cmd.Flags().StringVar(&triggerJSON, "trigger-json", "", "trigger payload as a JSON object")
	cmd.Flags().StringVar(&triggerFile, "trigger-file", "", "trigger payload from a JSON file (- for stdin)")
	cmd.Flags().BoolVar(&noPrompt, "no-prompt", false, "fail instead of prompting for missing trigger fields")
	cmd.Flags().BoolVar(&wait, "wait", false, "stream logs and wait for the run to finish (exit 1 unless it succeeds)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// parseValue turns "3", "true" or `{"a":1}` into typed values; anything
// else stays a string.
func parseValue(v string) any {
	var out any
	if err := json.Unmarshal([]byte(v), &out); err == nil {
		switch out.(type) {
		case float64, bool, map[string]any, []any:
			return out
		}
	}
	return v
}

func prompt(in io.Reader, out io.Writer, fields []string, trigger map[string]any) error {
	sc := bufio.NewScanner(in)
	for _, f := range fields {
		fmt.Fprintf(out, "trigger.%s: ", f)
		if !sc.Scan() {
			return errors.New("no input for trigger." + f)
		}
		trigger[f] = parseValue(strings.TrimSpace(sc.Text()))
	}
	return nil
}

// followRun streams a run's logs (one step, or all when step is "") until
// it ends, then returns its final state.
func followRun(ctx context.Context, c *apiclient.Client, runID, step string, w io.Writer) (*service.RunInfo, error) {
	var final *service.RunInfo
	err := c.Events(ctx, runID, true, func(ev apiclient.Event) bool {
		switch ev.Name {
		case "log":
			var le struct {
				Step string `json:"step"`
				Line string `json:"line"`
			}
			if json.Unmarshal(ev.Data, &le) == nil && (step == "" || le.Step == step) {
				if step == "" {
					fmt.Fprintf(w, "[%s] %s\n", le.Step, le.Line)
				} else {
					fmt.Fprintln(w, le.Line)
				}
			}
		case "end":
			var run service.RunInfo
			if json.Unmarshal(ev.Data, &run) == nil {
				final = &run
			}
		case "resync":
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if final == nil {
		// The stream ended early (resync); fall back to polling.
		return c.WaitRun(ctx, runID, 0)
	}
	return final, nil
}

func writePrefixed(w io.Writer, prefix string, b []byte) {
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		if len(line) > 0 {
			fmt.Fprintf(w, "[%s] %s", prefix, line)
			if line[len(line)-1] != '\n' {
				fmt.Fprintln(w)
			}
		}
	}
}

func triggerSummary(ts []workflow.TriggerSpec) string {
	var out []string
	for _, t := range ts {
		switch {
		case t.Type == workflow.TriggerCron:
			out = append(out, "cron("+t.Cron+")")
		case t.Type != "":
			out = append(out, t.Type)
		default:
			out = append(out, t.Integration+":"+t.Event)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "manual"
	}
	return strings.Join(out, ",")
}

func printRun(w io.Writer, run *service.RunInfo) {
	fmt.Fprintf(w, "run %s  workflow=%s v%d  status=%s  trigger=%s\n", run.ID, run.Workflow, run.Version, run.Status, run.TriggerKind)
	if run.Error != "" {
		fmt.Fprintf(w, "error: %s\n", run.Error)
	}
	if len(run.Steps) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  STEP\tNEEDS\tSTATUS\tATTEMPT\tEXIT\tOUTPUTS\tERROR")
	for _, st := range run.Steps {
		exit := "-"
		if st.ExitCode != nil {
			exit = strconv.Itoa(*st.ExitCode)
		}
		out := string(st.Outputs)
		if len(out) > 60 {
			out = out[:57] + "..."
		}
		needs := strings.Join(st.Needs, ",")
		if needs == "" {
			needs = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\t%s\n", st.ID, needs, st.Status, st.Attempt, exit, out, st.Error)
	}
	_ = tw.Flush()
}
