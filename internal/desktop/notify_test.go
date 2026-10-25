package desktop

import (
	"fmt"
	"testing"
)

func TestWatcherRunFailures(t *testing.T) {
	var w Watcher
	old := State{Reachable: true, Failed: []Run{{ID: "old", Workflow: "w"}}}
	if n := w.Observe(old); len(n) != 0 {
		t.Fatalf("failures from before the app started are not news: %+v", n)
	}
	if n := w.Observe(old); len(n) != 0 {
		t.Fatal("no change, no notification")
	}
	s := State{Reachable: true, Failed: []Run{{ID: "r/1", Workflow: "deploy", Error: "exit 1"}, {ID: "r2", Workflow: "lint"}, {ID: "old", Workflow: "w"}}}
	n := w.Observe(s)
	if len(n) != 2 || n[0].Title != "Run failed: deploy" || n[0].Body != "exit 1" || n[0].Action != (Action{Kind: ActOpenUI, Arg: "runs/r%2F1"}) {
		t.Fatalf("%+v", n)
	}
	if n[1].Body != "Run r2" || n[0].ID == n[1].ID {
		t.Fatalf("%+v", n)
	}
	if n := w.Observe(s); len(n) != 0 {
		t.Fatal("each failure once")
	}
	// A partial poll moves nothing: an empty list there is not "no failures".
	if n := w.Observe(State{Reachable: true, Err: "injected"}); len(n) != 0 {
		t.Fatal(n)
	}
	if n := w.Observe(s); len(n) != 0 {
		t.Fatal("still seen after a partial poll")
	}

	var many []Run
	for i := range groupFailures + 1 {
		many = append(many, Run{ID: fmt.Sprint("m", i), Workflow: fmt.Sprint("w", i)})
	}
	n = w.Observe(State{Reachable: true, Failed: many})
	if len(n) != 1 || n[0].Title != "4 runs failed" || n[0].Body != "w0 and 3 more" || n[0].Action.Arg != "runs" {
		t.Fatalf("%+v", n)
	}
}

func TestWatcherDaemonReachability(t *testing.T) {
	w := Watcher{Misses: 2}
	w.Observe(State{Reachable: true})
	down := State{Err: "connection refused"}
	if n := w.Observe(down); len(n) != 0 {
		t.Fatal("one miss is not an outage")
	}
	n := w.Observe(down)
	if len(n) != 1 || n[0].Title != "Ícaro daemon is not reachable" || n[0].Body != "connection refused" || n[0].Action.Kind != ActStartDaemon {
		t.Fatalf("%+v", n)
	}
	if n := w.Observe(down); len(n) != 0 {
		t.Fatal("announced once")
	}
	n = w.Observe(State{Reachable: true})
	if len(n) != 1 || n[0].ID != "daemon-back" {
		t.Fatalf("%+v", n)
	}
	// A failure that happened while the daemon was down is news.
	n = w.Observe(State{Reachable: true, Failed: []Run{{ID: "x", Workflow: "w", Error: "the daemon stopped before the container started"}}})
	if len(n) != 1 || n[0].ID != "run-failed-x" {
		t.Fatalf("%+v", n)
	}

	var remote Watcher // Misses 0 counts as 1
	if n := remote.Observe(State{Remote: true}); len(n) != 1 || n[0].Action.Kind != ActNone {
		t.Fatalf("a TCP daemon cannot be started from the app: %+v", n)
	}
}
