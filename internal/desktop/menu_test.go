package desktop

import (
	"fmt"
	"strings"
	"testing"
)

func TestMenuUnreachable(t *testing.T) {
	on := true
	m := Menu(State{Err: "dial unix /h/run/icaro.sock: connect: no such file or directory"}, Prefs{LoginItem: &on})
	got := labels(m)
	want := "Daemon not reachable; dial unix /h/run/icaro.sock: connect: no such file or direc…; Start daemon; Open Ícaro; ---; Launch at login; Tray only (no window at launch); Quit Ícaro"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if open, _ := find(m, "Open Ícaro"); !open.Disabled {
		t.Fatal("no UI without a daemon")
	}
	if login, _ := find(m, "Launch at login"); !login.Checkbox || !login.Checked {
		t.Fatalf("%+v", login)
	}
	// A TCP daemon is not the app's to start; no login item where unsupported.
	got = labels(Menu(State{Remote: true}, Prefs{TrayOnly: true}))
	if strings.Contains(got, "Start daemon") || strings.Contains(got, "Launch at login") {
		t.Fatal(got)
	}
	if tray, _ := find(Menu(State{}, Prefs{TrayOnly: true}), "Tray only (no window at launch)"); !tray.Checked {
		t.Fatal("tray-only is checked")
	}
}

func TestMenuRunsPreviewsUsage(t *testing.T) {
	s := State{
		Reachable: true,
		Running:   []Run{{ID: "r1", Workflow: "fix-ci", Status: "running"}},
		Waiting:   []Run{{ID: "r3", Workflow: "ship", Status: "waiting"}},
		Queued:    []Run{{ID: "r2", Workflow: "triage", Status: "queued"}},
		Failed:    []Run{{ID: "r/0", Workflow: "deploy", Status: "failed", Error: "step build: exit status 1"}, {ID: "r9", Workflow: "lint", Status: "failed"}},
		Previews: []Preview{
			{Name: "feat-x", Status: "running", URL: "http://feat-x.localhost"},
			{Name: "feat-y", Status: "blocked", Error: "privileged: true"},
			{Name: "feat-z", Status: "pending"},
		},
		Workflows: []Workflow{{Name: "fix-ci"}, {Name: "triage", NeedsInput: true}},
		Usage:     &Usage{Tokens: 1_234_567, CostUSD: 3.4},
	}
	m := Menu(s, Prefs{})
	got := labels(m)
	for _, want := range []string{
		"Ícaro is running; Open Ícaro; ---",
		"● fix-ci · running{Open run; Cancel run}",
		"◐ ship · waiting{Open run; Cancel run}; ○ triage · queued{Open run; Cancel run}",
		"Recent failures{✕ deploy — step build: exit status 1; ✕ lint}",
		"Run workflow{fix-ci; triage…}",
		"Previews; ● feat-x · running; ✕ feat-y · blocked — privileged: true; ○ feat-z · pending",
		"Today: 1.2M tokens · $3.40 est.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	cases := map[string]Action{
		"Cancel run": {Kind: ActCancelRun, Arg: "r1"},
		"✕ deploy — step build: exit status 1": {Kind: ActOpenUI, Arg: "runs/r%2F0"},
		"fix-ci":                          {Kind: ActRunWorkflow, Arg: "fix-ci"},
		"triage…":                         {Kind: ActOpenUI, Arg: "workflows/triage"},
		"● feat-x · running":              {Kind: ActOpenURL, Arg: "http://feat-x.localhost"},
		"Today: 1.2M tokens · $3.40 est.": {Kind: ActOpenUI, Arg: "usage"},
	}
	for label, want := range cases {
		if it, ok := find(m, label); !ok || it.Action != want {
			t.Errorf("%s: %+v", label, it)
		}
	}
	if it, _ := find(m, "✕ feat-y · blocked — privileged: true"); !it.Disabled {
		t.Fatal("a preview that is not running has nothing to open")
	}
	if Title(s) != "2" || Title(State{Reachable: true}) != "" || Title(State{}) != "!" {
		t.Fatal("title")
	}

	empty := labels(Menu(State{Reachable: true, Err: "injected"}, Prefs{}))
	for _, want := range []string{"No active runs", "Run workflow{No active workflows}", "No previews", "Partial data: injected"} {
		if !strings.Contains(empty, want) {
			t.Errorf("missing %q in %s", want, empty)
		}
	}
	if strings.Contains(empty, "Today") || strings.Contains(empty, "Recent failures") {
		t.Fatal(empty)
	}
}

func TestMenuCapsLongLists(t *testing.T) {
	var s State
	s.Reachable = true
	for i := range 7 {
		s.Running = append(s.Running, Run{ID: fmt.Sprint(i), Workflow: "w", Status: "running"})
		s.Failed = append(s.Failed, Run{ID: fmt.Sprint(i), Workflow: "w", Status: "failed"})
		s.Previews = append(s.Previews, Preview{Name: fmt.Sprint(i), Status: "stopped"})
	}
	m := Menu(s, Prefs{})
	if it, ok := find(m, "2 more…"); !ok || it.Action != (Action{Kind: ActOpenUI, Arg: "runs"}) {
		t.Fatalf("%s", labels(m))
	}
	failures, _ := find(m, "Recent failures")
	if len(failures.Children) != shownPerSection+1 {
		t.Fatal(labels(failures.Children))
	}
	if _, ok := find(m, "2 more previews (icaro preview ls)"); !ok {
		t.Fatal(labels(m))
	}
}

func TestUsageLabels(t *testing.T) {
	for _, c := range []struct {
		u    Usage
		want string
	}{
		{Usage{Tokens: 999}, "Today: 999 tokens · $0.00 est."},
		{Usage{Tokens: 1500, Unpriced: true}, "Today: 1.5k tokens"},
		{Usage{Tokens: 1500, Unpriced: true, CostUSD: 1}, "Today: 1.5k tokens · ≥$1.00 est."},
		{Usage{Tokens: 5, CostUSD: 6, Over: true, LimitUSD: 5}, "Today: 5 tokens · $6.00 est. · over the $5.00 limit"},
		{Usage{Err: "no transcripts"}, "Usage unavailable: no transcripts"},
	} {
		if got := usageItem(c.u).Label; got != c.want {
			t.Errorf("%+v: got %q, want %q", c.u, got, c.want)
		}
	}
	if statusMark("succeeded") != "·" || clip("a\n  b", 10) != "a b" {
		t.Fatal("helpers")
	}
}
