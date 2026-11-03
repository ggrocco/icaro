package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"icaro/internal/apiclient/rest"
)

// The run and workflow states the tray asks for (store's values).
const (
	runRunning     = "running"
	runQueued      = "queued"
	runWaiting     = "waiting"
	runFailed      = "failed"
	workflowActive = "active"
)

// The parts of the daemon's responses the tray reads. The app decodes them
// here rather than through package apiclient, whose types would link the
// whole daemon into it.
type (
	runInfo struct {
		ID       string `json:"id"`
		Workflow string `json:"workflow"`
		Status   string `json:"status"`
		Error    string `json:"error"`
	}
	previewInfo struct {
		ID           string `json:"id"`
		WorktreePath string `json:"worktree_path"`
		Status       string `json:"status"`
		URL          string `json:"url"`
		Error        string `json:"error"`
	}
	workflowInfo struct {
		Name          string `json:"name"`
		Status        string `json:"status"`
		TriggerFields struct {
			Required []string `json:"required"`
		} `json:"trigger_fields"`
	}
	usageReport struct {
		Timezone string  `json:"timezone"`
		LimitUSD float64 `json:"daily_limit_usd"`
		Records  []struct {
			Day              string   `json:"day"`
			InputTokens      int64    `json:"input_tokens"`
			OutputTokens     int64    `json:"output_tokens"`
			CacheReadTokens  int64    `json:"cache_read_tokens"`
			CacheWriteTokens int64    `json:"cache_write_tokens"`
			CostUSD          *float64 `json:"cost_usd_estimate"`
			Error            string   `json:"error"`
		} `json:"records"`
		Alerts []struct {
			Day string `json:"day"`
		} `json:"alerts"`
	}
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
	Waiting   []Run // parked on an approval or a wait step
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
	st.Running, err = runs(ctx, c, runRunning, listLimit)
	keep(err)
	st.Waiting, err = runs(ctx, c, runWaiting, listLimit)
	keep(err)
	st.Queued, err = runs(ctx, c, runQueued, listLimit)
	keep(err)
	st.Failed, err = runs(ctx, c, runFailed, failedLimit)
	keep(err)
	var previews struct {
		Previews []previewInfo `json:"previews"`
	}
	keep(c.Do(ctx, http.MethodGet, "/api/v1/previews", nil, &previews))
	for _, p := range previews.Previews {
		st.Previews = append(st.Previews, Preview{ID: p.ID, Name: filepath.Base(p.WorktreePath), Status: p.Status, URL: p.URL, Error: p.Error})
	}
	var wfs struct {
		Workflows []workflowInfo `json:"workflows"`
	}
	keep(c.Do(ctx, http.MethodGet, "/api/v1/workflows", nil, &wfs))
	for _, w := range wfs.Workflows {
		if w.Status == workflowActive {
			st.Workflows = append(st.Workflows, Workflow{Name: w.Name, NeedsInput: len(w.TriggerFields.Required) > 0})
		}
	}
	return st
}

func runs(ctx context.Context, c *rest.Client, status string, limit int) ([]Run, error) {
	q := url.Values{"status": {status}, "limit": {strconv.Itoa(limit)}}
	var infos struct {
		Runs []runInfo `json:"runs"`
	}
	err := c.Do(ctx, http.MethodGet, "/api/v1/runs?"+q.Encode(), nil, &infos)
	out := make([]Run, 0, len(infos.Runs))
	for _, r := range infos.Runs {
		out = append(out, Run(r))
	}
	return out, err
}

// FetchUsage summarizes today's usage. Parsing transcripts costs more
// than the other queries, so the controller polls it less often.
func FetchUsage(ctx context.Context, d *Daemon, now time.Time) *Usage {
	var rep usageReport
	if err := d.Client.Do(ctx, http.MethodGet, "/api/v1/usage?since=0d", nil, &rep); err != nil {
		return &Usage{Err: err.Error()}
	}
	return summarize(&rep, now)
}

func summarize(rep *usageReport, now time.Time) *Usage {
	loc := time.Local
	if rep.Timezone != "" && rep.Timezone != "Local" && rep.Timezone != "local" {
		if l, err := time.LoadLocation(rep.Timezone); err == nil {
			loc = l
		}
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
