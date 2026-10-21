package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"icaro/internal/usage"
)

func init() { register(newUsageCmd) }

func newUsageCmd(opts *rootOpts) *cobra.Command {
	var sinceFlag string
	var asJSON, hostClaude, noSave bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show agent token usage parsed from local logs (costs are estimates)",
		Long: `Parses the Claude Code transcripts in every profile home (and, with
--host-claude, your own ~/.claude, read-only) into per-day, per-model token
counts. Each row names its source and confidence. Costs come from a list-price
table and are always estimates; unpriced models show no cost. Alerts fire when
a day's estimated spend reaches usage.daily_limit_usd, in usage.timezone.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}
			roots := usage.ProfileRoots(cfg.Layout().Homes())
			if hostClaude {
				if home, err := os.UserHomeDir(); err == nil {
					roots = append(roots, usage.Root{Label: "host", Dir: filepath.Join(home, ".claude"), Required: true})
				}
			}
			rep, err := usage.BuildReport(cmd.Context(), usage.ReportOptions{
				Roots: roots, Timezone: cfg.Usage.Timezone, Since: sinceFlag, LimitUSD: cfg.Usage.DailyLimit,
			})
			if err != nil {
				return err
			}
			if !noSave {
				if st, err := openStore(cmd.Context(), cfg); err == nil {
					if err := st.Migrate(cmd.Context()); err != nil {
						fmt.Fprintln(cmd.ErrOrStderr(), "warning: could not migrate the database:", err)
					} else if err := usage.Save(cmd.Context(), st, rep.Records); err != nil {
						fmt.Fprintln(cmd.ErrOrStderr(), "warning: could not save usage snapshots:", err)
					}
					_ = st.Close()
				}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			printUsage(out, rep.Records)
			for _, a := range rep.Alerts {
				more := ""
				if a.Partial {
					more = " (at least; some usage is unpriced)"
				}
				fmt.Fprintf(out, "ALERT %s: estimated spend $%.2f reached the daily limit $%.2f%s\n", a.Day, a.SpentUSD, a.LimitUSD, more)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sinceFlag, "since", usage.DefaultSince, "window start: a duration in days (7d) or a date (2025-03-01)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&hostClaude, "host-claude", false, "also read your own ~/.claude transcripts (read-only)")
	cmd.Flags().BoolVar(&noSave, "no-save", false, "do not store snapshots in the database")
	return cmd
}

func printUsage(w interface{ Write([]byte) (int, error) }, recs []usage.Record) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DAY\tSOURCE\tMODEL\tMSGS\tINPUT\tOUTPUT\tCACHE R/W\tCOST (EST.)\tCONFIDENCE")
	for _, r := range recs {
		if r.Error != "" {
			fmt.Fprintf(tw, "-\t%s\t-\t-\t-\t-\t-\t-\t%s: %s\n", r.Source, r.Confidence, r.Error)
			continue
		}
		cost := "n/a"
		if r.CostUSD != nil {
			cost = fmt.Sprintf("~$%.4f", *r.CostUSD)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d/%d\t%s\t%s\n", r.Day, r.Source, r.Model, r.Messages, r.InputTokens, r.OutputTokens,
			r.CacheReadTokens, r.CacheWriteTokens, cost, r.Confidence)
	}
	_ = tw.Flush()
}
