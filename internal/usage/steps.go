package usage

import (
	"context"
	"fmt"
	"time"

	"icaro/internal/store"
)

// AgentSteps reports the token usage workflow agent steps recorded from
// their CLI's event stream (ADR 0037). A step's home layer is discarded
// when it ends, so its transcript is gone; the step run is the record.
// Subscription runs have tokens only; inject (API-key) runs carry the
// CLI's own cost figure, which is still an estimate.
type AgentSteps struct{ Store *store.Store }

// Name implements Provider.
func (a *AgentSteps) Name() string { return "agent-steps" }

// Collect implements Provider: one record per step run, sourced as the
// run and step, under the day the step started.
func (a *AgentSteps) Collect(ctx context.Context, since time.Time, loc *time.Location) ([]Record, error) {
	rows, err := a.Store.ListAgentStepUsage(ctx, since)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range rows {
		u := r.Usage
		if u.Input+u.Output+u.CacheRead+u.CacheWrite == 0 {
			continue
		}
		rec := Record{
			Provider: a.Name(), Source: fmt.Sprintf("%s run %s step %s", r.Workflow, r.RunID, r.StepID),
			Day: r.StartedAt.In(loc).Format("2006-01-02"), Model: "profile " + r.Profile,
			InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite,
			Messages: 1, Confidence: ConfidenceHigh,
		}
		if u.CostMicro > 0 {
			c := float64(u.CostMicro) / 1e6
			rec.CostUSD = &c
		}
		out = append(out, rec)
	}
	return out, nil
}
