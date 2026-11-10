package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"icaro/internal/assist/distill"
	"icaro/internal/session"
)

// newMemoryModelCmd manages the local models the memory distiller runs
// (ADR 0036). It works on ~/.icaro/models directly; the daemon picks a
// pulled model up on its next job.
func newMemoryModelCmd(opts *rootOpts, asJSON *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Manage the local model that summarizes assistant chats (optional)",
		Long: "Without a model, assistant chats leave exact memories only (validator lessons, integration facts, run and save outcomes). " +
			"A pulled model also summarizes each chat's intent, constraints and where it left off. It runs on the CPU in a container " +
			"with no network; GPU users can set memory.distill.endpoint to their own llama-server or Ollama instead.",
	}
	store := func() (distill.Store, string, error) {
		cfg, err := opts.loadConfig()
		if err != nil {
			return distill.Store{}, "", err
		}
		return distill.Store{Dir: cfg.Layout().Models(), BaseURL: modelsBaseURL}, distill.AutoTier(int64(min(session.TotalRAM(), 1<<62))), nil
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List the models you can pull, and which are pulled",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, tier, err := store()
			if err != nil {
				return err
			}
			pulled := map[string]bool{}
			for _, m := range s.Installed() {
				pulled[m.Name] = true
			}
			if *asJSON {
				type row struct {
					distill.Model
					Pulled bool `json:"pulled"`
				}
				out := []row{}
				for _, m := range distill.Catalog {
					out = append(out, row{m, pulled[m.Name]})
				}
				return printJSON(cmd.OutOrStdout(), map[string]any{"tier": tier, "models": out})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "This machine's tier: %s (default model %s)\n\n", tier, distill.Default(tier).Name)
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTIER\tSIZE\tLICENSE\tPULLED\tSOURCE")
			for _, m := range distill.Catalog {
				mark := "-"
				if pulled[m.Name] {
					mark = "yes"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s/%s\n", m.Name, m.Tier, humanBytes(m.Size), m.License, mark, m.Repo, m.File)
			}
			return tw.Flush()
		},
	}

	pull := &cobra.Command{
		Use:   "pull [NAME]",
		Short: "Download a model (default: the one for this machine's tier) and check its pinned SHA-256",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, tier, err := store()
			if err != nil {
				return err
			}
			m := distill.Default(tier)
			if len(args) == 1 {
				if m, err = distill.Lookup(args[0]); err != nil {
					return err
				}
			}
			out := cmd.ErrOrStderr()
			fmt.Fprintf(out, "pulling %s (%s) from %s\n", m.Name, humanBytes(m.Size), m.Repo)
			last := time.Time{}
			err = s.Pull(cmd.Context(), m, func(done, total int64) {
				if time.Since(last) > 2*time.Second || done == total {
					last = time.Now()
					fmt.Fprintf(out, "  %s of %s\n", humanBytes(done), humanBytes(total))
				}
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pulled %s; its SHA-256 matches the pin\n", m.Name)
			return nil
		},
	}

	var yes bool
	rm := &cobra.Command{
		Use:   "rm NAME",
		Short: "Delete a pulled model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := store()
			if err != nil {
				return err
			}
			m, err := distill.Lookup(args[0])
			if err != nil {
				return err
			}
			if !yes {
				ok, err := confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), fmt.Sprintf("Delete model %s (%s)?", m.Name, humanBytes(m.Size)))
				if err != nil || !ok {
					return err
				}
			}
			return s.Remove(m)
		},
	}
	rm.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.AddCommand(list, pull, rm)
	return cmd
}

// modelsBaseURL is where models are downloaded from; empty means Hugging
// Face (tests point it at a fake).
var modelsBaseURL string

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
