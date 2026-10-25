package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/store"
	"icaro/internal/usage"
)

// State is one poll of the daemon: everything the tray shows. It is built
// from the daemon's API alone, so after a restart (of the app or of the
// daemon) the next poll shows the same thing the web UI does.
type State struct {
	Reachable bool
	// Err is why the daemon is unreachable, or the first failed query of
	// a reachable one.
	Err string
	// Remote is a TCP daemon (daemon.url), which the app does not start.
	Remote    bool
	Running   []Run
	Queued    []Run
	Failed    []Run // the most recent first
	Previews  []Preview
	Workflows []Workflow
	Usage     *Usage // nil until the first usage poll
}

// Run is a run as the tray lists it.
type Run struct {
	ID       string
	Workflow string
	Status   string
	Error    string
}

// Preview is a preview as the tray lists it.
type Preview struct {
	ID     string
	Name   string // the worktree directory
	Status string
	URL    string
	Error  string
}

// Workflow is an active workflow the tray can start.
type Workflow struct {
	Name string
	// NeedsInput is set when a manual run must supply trigger fields;
	// the tray then opens the workflow in the UI instead of running it.
	NeedsInput bool
}

// Usage is today's usage in the report's time zone.
type Usage struct {
	Day      string
	Tokens   int64
	CostUSD  float64
	Unpriced bool // some usage had no price, so the cost is a floor
	LimitUSD float64
	Over     bool // a daily-limit alert fired for today
	Err      string
}

// Limits of what one poll fetches; the tray shows fewer.
const (
	listLimit   = 20
	failedLimit = 10
)

// Fetch polls the daemon. It never fails: an unreachable daemon is a
// State too.
func Fetch(ctx context.Context, d *Daemon) State {
	st := State{Remote: d.Remote}
	if err := d.Healthy(ctx); err != nil {
		st.Err = err.Error()
		return st
	}
	st.Reachable = true
	c := d.Client
	keep := func(err error) {
		if err != nil && st.Err == "" {
			st.Err = err.Error()
		}
	}
	var err error
	st.Running, err = runs(ctx, c, store.RunRunning, listLimit)
	keep(err)
	st.Queued, err = runs(ctx, c, store.RunQueued, listLimit)
	keep(err)
	st.Failed, err = runs(ctx, c, store.RunFailed, failedLimit)
	keep(err)
	previews, err := c.ListPreviews(ctx)
	keep(err)
	for _, p := range previews {
		st.Previews = append(st.Previews, Preview{ID: p.ID, Name: filepath.Base(p.WorktreePath), Status: p.Status, URL: p.URL, Error: p.Error})
	}
	wfs, err := c.ListWorkflows(ctx)
	keep(err)
	for _, w := range wfs {
		if w.Status == store.WorkflowActive {
			st.Workflows = append(st.Workflows, Workflow{Name: w.Name, NeedsInput: len(w.TriggerFields.Required) > 0})
		}
	}
	return st
}

func runs(ctx context.Context, c *apiclient.Client, status string, limit int) ([]Run, error) {
	infos, err := c.ListRuns(ctx, "", status, limit)
	out := make([]Run, 0, len(infos))
	for _, r := range infos {
		out = append(out, Run{ID: r.ID, Workflow: r.Workflow, Status: r.Status, Error: r.Error})
	}
	return out, err
}

// FetchUsage summarizes today's usage. Parsing transcripts costs more
// than the other queries, so the controller polls it less often.
func FetchUsage(ctx context.Context, d *Daemon, now time.Time) *Usage {
	rep, err := d.Client.Usage(ctx, "0d")
	if err != nil {
		return &Usage{Err: err.Error()}
	}
	return summarize(rep, now)
}

func summarize(rep *usage.Report, now time.Time) *Usage {
	loc, err := usage.Location(rep.Timezone)
	if err != nil {
		loc = time.Local
	}
	u := &Usage{Day: now.In(loc).Format(time.DateOnly), LimitUSD: rep.LimitUSD}
	var errs []error
	for _, r := range rep.Records {
		if r.Error != "" {
			errs = append(errs, errors.New(r.Error))
			continue
		}
		if r.Day != u.Day {
			continue
		}
		u.Tokens += r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens
		if r.CostUSD != nil {
			u.CostUSD += *r.CostUSD
		} else {
			u.Unpriced = true
		}
	}
	for _, a := range rep.Alerts {
		if a.Day == u.Day {
			u.Over = true
		}
	}
	if u.Tokens == 0 && len(errs) > 0 {
		u.Err = errors.Join(errs...).Error()
	}
	return u
}
