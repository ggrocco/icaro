package desktop

import (
	"fmt"
	"net/url"
	"strings"
)

// ActionKind is what a menu item does.
type ActionKind string

// Menu actions.
const (
	ActNone        ActionKind = ""
	ActOpenUI      ActionKind = "open-ui"   // Arg: the UI route, e.g. "runs/<id>"; empty is the home page
	ActOpenURL     ActionKind = "open-url"  // Arg: a preview URL
	ActCancelRun   ActionKind = "cancel"    // Arg: run id
	ActRunWorkflow ActionKind = "run"       // Arg: workflow name
	ActStartDaemon ActionKind = "start"     //
	ActLoginItem   ActionKind = "login"     // toggles launch at login
	ActTrayOnly    ActionKind = "tray-only" // toggles tray-only mode
	ActQuit        ActionKind = "quit"      //
)

// Action is a menu item's effect.
type Action struct {
	Kind ActionKind
	Arg  string
}

// Item is one entry of the tray menu. The shell renders it as is.
type Item struct {
	Label     string
	Disabled  bool
	Separator bool
	Checkbox  bool
	Checked   bool
	Action    Action
	Children  []Item
}

// Prefs are the toggles shown in the menu.
type Prefs struct {
	// LoginItem is nil where launch at login is not supported.
	LoginItem *bool
	TrayOnly  bool
}

// Shown at most per section; the rest is one "more" entry into the UI.
const shownPerSection = 5

// Menu builds the tray menu for a state.
func Menu(s State, p Prefs) []Item {
	var m []Item
	if s.Reachable {
		m = append(m, Item{Label: "Ícaro is running", Disabled: true})
	} else {
		m = append(m, Item{Label: "Daemon not reachable", Disabled: true})
		if s.Err != "" {
			m = append(m, Item{Label: clip(s.Err, 60), Disabled: true})
		}
		if !s.Remote {
			m = append(m, Item{Label: "Start daemon", Action: Action{Kind: ActStartDaemon}})
		}
	}
	m = append(m, Item{Label: "Open Ícaro", Action: Action{Kind: ActOpenUI}, Disabled: !s.Reachable})
	if s.Reachable {
		m = append(m, sep())
		m = append(m, runsSection(s)...)
		m = append(m, sep())
		m = append(m, previewsSection(s.Previews)...)
		if s.Usage != nil {
			m = append(m, sep(), usageItem(*s.Usage))
		}
		if s.Err != "" {
			m = append(m, sep(), Item{Label: "Partial data: " + clip(s.Err, 50), Disabled: true})
		}
	}
	m = append(m, sep())
	if p.LoginItem != nil {
		m = append(m, Item{Label: "Launch at login", Checkbox: true, Checked: *p.LoginItem, Action: Action{Kind: ActLoginItem}})
	}
	m = append(m, Item{Label: "Tray only (no window at launch)", Checkbox: true, Checked: p.TrayOnly, Action: Action{Kind: ActTrayOnly}})
	m = append(m, Item{Label: "Quit Ícaro", Action: Action{Kind: ActQuit}})
	return m
}

func runsSection(s State) []Item {
	var m []Item
	active := append(append(append([]Run{}, s.Running...), s.Waiting...), s.Queued...)
	if len(active) == 0 {
		m = append(m, Item{Label: "No active runs", Disabled: true})
	}
	for i, r := range active {
		if i == shownPerSection {
			m = append(m, more(len(active)-i, "runs"))
			break
		}
		m = append(m, Item{Label: fmt.Sprintf("%s %s · %s", statusMark(r.Status), r.Workflow, r.Status), Children: []Item{
			{Label: "Open run", Action: Action{Kind: ActOpenUI, Arg: "runs/" + url.PathEscape(r.ID)}},
			{Label: "Cancel run", Action: Action{Kind: ActCancelRun, Arg: r.ID}},
		}})
	}
	if len(s.Failed) > 0 {
		var failed []Item
		for i, r := range s.Failed {
			if i == shownPerSection {
				failed = append(failed, more(len(s.Failed)-i, "runs"))
				break
			}
			label := statusMark(r.Status) + " " + r.Workflow
			if r.Error != "" {
				label += " — " + clip(r.Error, 40)
			}
			failed = append(failed, Item{Label: label, Action: Action{Kind: ActOpenUI, Arg: "runs/" + url.PathEscape(r.ID)}})
		}
		m = append(m, Item{Label: "Recent failures", Children: failed})
	}
	var run []Item
	for _, w := range s.Workflows {
		if w.NeedsInput {
			run = append(run, Item{Label: w.Name + "…", Action: Action{Kind: ActOpenUI, Arg: "workflows/" + url.PathEscape(w.Name)}})
		} else {
			run = append(run, Item{Label: w.Name, Action: Action{Kind: ActRunWorkflow, Arg: w.Name}})
		}
	}
	if len(run) == 0 {
		run = []Item{{Label: "No active workflows", Disabled: true}}
	}
	return append(m, Item{Label: "Run workflow", Children: run})
}

func previewsSection(ps []Preview) []Item {
	if len(ps) == 0 {
		return []Item{{Label: "No previews", Disabled: true}}
	}
	items := make([]Item, 0, len(ps))
	for i, p := range ps {
		if i == shownPerSection {
			items = append(items, Item{Label: fmt.Sprintf("%d more previews (icaro preview ls)", len(ps)-i), Disabled: true})
			break
		}
		it := Item{Label: fmt.Sprintf("%s %s · %s", statusMark(p.Status), p.Name, p.Status), Disabled: true}
		if p.Status == "running" && p.URL != "" {
			it.Disabled, it.Action = false, Action{Kind: ActOpenURL, Arg: p.URL}
		} else if p.Error != "" {
			it.Label += " — " + clip(p.Error, 40)
		}
		items = append(items, it)
	}
	return append([]Item{{Label: "Previews", Disabled: true}}, items...)
}

func usageItem(u Usage) Item {
	open := Action{Kind: ActOpenUI, Arg: "usage"}
	if u.Err != "" {
		return Item{Label: "Usage unavailable: " + clip(u.Err, 40), Action: open}
	}
	label := "Today: " + tokens(u.Tokens) + " tokens"
	if u.CostUSD > 0 || !u.Unpriced {
		floor := ""
		if u.Unpriced {
			floor = "≥"
		}
		label += fmt.Sprintf(" · %s$%.2f est.", floor, u.CostUSD)
	}
	if u.Over {
		label += fmt.Sprintf(" · over the $%.2f limit", u.LimitUSD)
	}
	return Item{Label: label, Action: open}
}

func more(n int, route string) Item {
	return Item{Label: fmt.Sprintf("%d more…", n), Action: Action{Kind: ActOpenUI, Arg: route}}
}

func sep() Item { return Item{Separator: true} }

func statusMark(status string) string {
	switch status {
	case "running":
		return "●"
	case "queued", "pending":
		return "○"
	case "waiting":
		return "◐"
	case "failed", "error", "blocked":
		return "✕"
	}
	return "·"
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// clip keeps a menu label to one short line.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Title is the text next to the tray icon (macOS) or its tooltip: the
// number of active runs, and a mark when something needs attention.
func Title(s State) string {
	switch {
	case !s.Reachable:
		return "!"
	case len(s.Running)+len(s.Queued) > 0:
		return fmt.Sprint(len(s.Running) + len(s.Queued))
	}
	return ""
}
