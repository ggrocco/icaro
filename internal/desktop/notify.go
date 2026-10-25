package desktop

import (
	"fmt"
	"net/url"
)

// Notification is a native notification the shell posts.
type Notification struct {
	// ID is stable per event, so the notification center replaces
	// rather than stacks repeats.
	ID     string
	Title  string
	Body   string
	Action Action // what clicking it does
}

// Watcher turns successive states into notifications: a run that failed
// since the last poll, and the daemon becoming unreachable (and back).
type Watcher struct {
	// Misses is how many failed polls in a row make the daemon count as
	// unreachable; one slow answer is not an outage. Values below 1 mean 1.
	Misses int

	baseline bool
	seen     map[string]bool
	misses   int
	down     bool
}

// More new failures than this in one poll become a single notification.
const groupFailures = 3

// Observe returns the notifications for a new state.
func (w *Watcher) Observe(s State) []Notification {
	if !s.Reachable {
		w.misses++
		if w.down || w.misses < max(w.Misses, 1) {
			return nil
		}
		w.down = true
		n := Notification{ID: "daemon-unreachable", Title: "Ícaro daemon is not reachable", Body: clip(s.Err, 200)}
		if !s.Remote {
			n.Action = Action{Kind: ActStartDaemon}
		}
		return []Notification{n}
	}
	w.misses = 0
	var out []Notification
	if w.down {
		w.down = false
		out = append(out, Notification{ID: "daemon-back", Title: "Ícaro daemon is reachable again", Action: Action{Kind: ActOpenUI}})
	}
	// A partial poll may have missed the failed runs; an empty list then
	// says nothing, so neither the baseline nor the diff moves.
	if s.Err != "" {
		return out
	}
	current := make(map[string]bool, len(s.Failed))
	var fresh []Run
	for _, r := range s.Failed {
		current[r.ID] = true
		if w.baseline && !w.seen[r.ID] {
			fresh = append(fresh, r)
		}
	}
	// Failures from before the app started are history, not news. The
	// list is newest first, so a run that drops off it never returns.
	w.baseline, w.seen = true, current
	if len(fresh) > groupFailures {
		return append(out, Notification{ID: "runs-failed", Title: fmt.Sprintf("%d runs failed", len(fresh)),
			Body: fmt.Sprintf("%s and %d more", fresh[0].Workflow, len(fresh)-1), Action: Action{Kind: ActOpenUI, Arg: "runs"}})
	}
	for _, r := range fresh {
		body := r.Error
		if body == "" {
			body = "Run " + r.ID
		}
		out = append(out, Notification{ID: "run-failed-" + r.ID, Title: "Run failed: " + r.Workflow, Body: clip(body, 200),
			Action: Action{Kind: ActOpenUI, Arg: "runs/" + url.PathEscape(r.ID)}})
	}
	return out
}
