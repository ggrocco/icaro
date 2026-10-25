package main

import (
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"

	"icaro/internal/desktop"
)

// A notification's data comes back from the OS notification center; only
// non-destructive actions are honored from it.
func TestActionFromData(t *testing.T) {
	open := desktop.Action{Kind: desktop.ActOpenUI, Arg: "runs/r1"}
	if got := actionFromData(actionData(open)); got != open {
		t.Fatal(got)
	}
	start := desktop.Action{Kind: desktop.ActStartDaemon}
	if got := actionFromData(actionData(start)); got != start {
		t.Fatal(got)
	}
	for _, kind := range []desktop.ActionKind{desktop.ActCancelRun, desktop.ActRunWorkflow, desktop.ActQuit, desktop.ActOpenURL} {
		if got := actionFromData(map[string]any{"action": string(kind), "arg": "x"}); got != (desktop.Action{Kind: desktop.ActOpenUI}) {
			t.Errorf("%s from a notification: %+v", kind, got)
		}
	}
	if actionData(desktop.Action{}) != nil {
		t.Fatal("no action, no data")
	}
	if got := actionFromData(nil); got.Kind != desktop.ActOpenUI {
		t.Fatal(got)
	}
}

func TestBuildMenu(t *testing.T) {
	items := desktop.Menu(desktop.State{Reachable: true, Running: []desktop.Run{{ID: "r1", Workflow: "w", Status: "running"}}}, desktop.Prefs{})
	m := application.NewMenu()
	var got []desktop.Action
	buildMenu(m, items, func(a desktop.Action) { got = append(got, a) })
	if m.FindByLabel("Open Ícaro") == nil || m.FindByLabel("Cancel run") == nil {
		t.Fatal("items and submenus are rendered")
	}
	if !strings.Contains(strings.Join(labels(items), "|"), "● w · running ▸ Open run | Cancel run") {
		t.Fatal(labels(items))
	}
	if m.FindByLabel("Ícaro is running").Enabled() {
		t.Fatal("disabled items stay disabled")
	}
	if !m.FindByLabel("Tray only (no window at launch)").IsCheckbox() {
		t.Fatal("checkboxes are checkboxes")
	}
}
