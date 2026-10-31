package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/usage"
)

func cost(v float64) *float64 { return &v }

func TestFetch(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.set(func(f *fakeAPI) {
		f.runs[store.RunRunning] = []*service.RunInfo{{ID: "r1", Workflow: "fix-ci", Status: store.RunRunning}}
		f.runs[store.RunQueued] = []*service.RunInfo{{ID: "r2", Workflow: "triage", Status: store.RunQueued}}
		f.runs[store.RunWaiting] = []*service.RunInfo{{ID: "r3", Workflow: "ship", Status: store.RunWaiting}}
		f.runs[store.RunFailed] = []*service.RunInfo{{ID: "r0", Workflow: "deploy", Status: store.RunFailed, Error: "exit 1"}}
		f.previews = []apiclient.Preview{{PreviewInstance: store.PreviewInstance{ID: "p1", WorktreePath: "/w/feat-x", Status: store.PreviewRunning, URL: "http://feat-x.localhost"}}}
		f.workflows = []*service.WorkflowInfo{
			{Name: "fix-ci", Status: store.WorkflowActive},
			{Name: "triage", Status: store.WorkflowActive, TriggerFields: service.TriggerFields{Required: []string{"issue"}}},
			{Name: "old", Status: store.WorkflowDisabled},
		}
	})
	st := Fetch(context.Background(), remoteDaemon(srv))
	if !st.Reachable || st.Err != "" || !st.Remote {
		t.Fatalf("%+v", st)
	}
	if len(st.Running) != 1 || st.Running[0].Workflow != "fix-ci" || len(st.Queued) != 1 || len(st.Waiting) != 1 || st.Failed[0].Error != "exit 1" {
		t.Fatalf("runs: %+v", st)
	}
	if len(st.Previews) != 1 || st.Previews[0].Name != "feat-x" || st.Previews[0].URL == "" {
		t.Fatalf("previews: %+v", st.Previews)
	}
	if len(st.Workflows) != 2 || st.Workflows[0].NeedsInput || !st.Workflows[1].NeedsInput {
		t.Fatalf("only active workflows, flagged when they need input: %+v", st.Workflows)
	}
	for _, q := range []string{"GET /api/v1/runs", "GET /api/v1/previews", "GET /api/v1/workflows"} {
		if !strings.Contains(strings.Join(f.called(), ","), q) {
			t.Errorf("%s not queried", q)
		}
	}

	// One failed query keeps the rest and says so.
	f.set(func(f *fakeAPI) { f.fail["GET /api/v1/previews"] = http.StatusInternalServerError })
	st = Fetch(context.Background(), remoteDaemon(srv))
	if !st.Reachable || !strings.Contains(st.Err, "injected") || len(st.Running) != 1 {
		t.Fatalf("%+v", st)
	}

	f.set(func(f *fakeAPI) { f.down = true })
	st = Fetch(context.Background(), remoteDaemon(srv))
	if st.Reachable || st.Err == "" || len(st.Running) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestUsageSummary(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)
	rep := &usage.Report{Timezone: "America/Sao_Paulo", LimitUSD: 5, Records: []usage.Record{
		// 22:30 the previous day in São Paulo is "today" there.
		{Day: "2026-10-24", InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 100, CacheWriteTokens: 50, CostUSD: cost(1.25)},
		{Day: "2026-10-24", InputTokens: 10, Model: "unpriced"},
		{Day: "2026-10-23", InputTokens: 99999, CostUSD: cost(9)},
		{Error: "source unavailable: /h/x"},
	}, Alerts: []usage.Alert{{Day: "2026-10-23"}}}
	u := summarize(asWire(t, rep), now)
	if u.Day != "2026-10-24" || u.Tokens != 1660 || u.CostUSD != 1.25 || !u.Unpriced || u.Over || u.Err != "" || u.LimitUSD != 5 {
		t.Fatalf("%+v", u)
	}
	rep.Alerts = append(rep.Alerts, usage.Alert{Day: "2026-10-24"})
	if u := summarize(asWire(t, rep), now); !u.Over {
		t.Fatal("today's alert")
	}
	// Nothing today and a broken source: the error is the news.
	rep = &usage.Report{Timezone: "bogus/zone", Records: []usage.Record{{Error: "unreadable"}}}
	if u := summarize(asWire(t, rep), now); u.Err != "unreadable" || u.Day != now.In(time.Local).Format(time.DateOnly) {
		t.Fatalf("%+v", u)
	}

	f, srv := newFakeAPI(t)
	f.set(func(f *fakeAPI) {
		f.usage = usage.Report{Timezone: "UTC", Records: []usage.Record{{Day: "2026-10-25", OutputTokens: 7, CostUSD: cost(0.01)}}}
	})
	if u := FetchUsage(context.Background(), remoteDaemon(srv), now); u.Tokens != 7 || u.Err != "" {
		t.Fatalf("%+v", u)
	}
	f.set(func(f *fakeAPI) { f.fail["GET /api/v1/usage"] = http.StatusBadRequest })
	if u := FetchUsage(context.Background(), remoteDaemon(srv), now); !strings.Contains(u.Err, "injected") {
		t.Fatalf("%+v", u)
	}
}

// asWire sends a report through JSON, as the daemon does, into the
// fields the tray reads.
func asWire(t *testing.T, rep *usage.Report) *usageReport {
	t.Helper()
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var out usageReport
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// The tray names states without importing store, which would link the
// database drivers into the app.
func TestStatesMatchTheStore(t *testing.T) {
	for got, want := range map[string]string{runRunning: store.RunRunning, runQueued: store.RunQueued, runWaiting: store.RunWaiting, runFailed: store.RunFailed, workflowActive: store.WorkflowActive} {
		if got != want {
			t.Errorf("%q != store's %q", got, want)
		}
	}
}
