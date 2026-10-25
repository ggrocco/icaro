package main

import (
	"log/slog"
	"runtime"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"icaro/internal/desktop"
)

// shell implements desktop.UI with Wails. Every call is moved to the main
// thread, where Wails requires menus and windows to be touched.
type shell struct {
	app   *application.App
	tray  *application.SystemTray
	menu  *application.Menu
	notes *notifications.NotificationService
	act   func(desktop.Action)

	mu     sync.Mutex
	window *application.WebviewWindow
}

func (s *shell) SetMenu(items []desktop.Item, title string) {
	slog.Debug("menu", "title", title, "items", labels(items))
	application.InvokeAsync(func() {
		s.menu.Clear()
		buildMenu(s.menu, items, s.act)
		s.menu.Update()
		tooltip := "Ícaro"
		if title != "" {
			tooltip += " (" + title + ")"
		}
		s.tray.SetTooltip(tooltip)
		if runtime.GOOS == "darwin" {
			s.tray.SetLabel(title)
		}
	})
}

// buildMenu renders the menu model. Wails toggles a checkbox on click;
// the controller then re-renders it from the real setting.
func buildMenu(m *application.Menu, items []desktop.Item, act func(desktop.Action)) {
	for _, it := range items {
		switch {
		case it.Separator:
			m.AddSeparator()
		case len(it.Children) > 0:
			buildMenu(m.AddSubmenu(it.Label), it.Children, act)
		default:
			var mi *application.MenuItem
			if it.Checkbox {
				mi = m.AddCheckbox(it.Label, it.Checked)
			} else {
				mi = m.Add(it.Label)
			}
			mi.SetEnabled(!it.Disabled)
			if a := it.Action; a.Kind != desktop.ActNone {
				mi.OnClick(func(*application.Context) { act(a) })
			}
		}
	}
}

func (s *shell) Notify(n desktop.Notification) {
	slog.Debug("notify", "id", n.ID, "title", n.Title, "body", n.Body)
	err := s.notes.SendNotification(notifications.NotificationOptions{
		ID: n.ID, Title: n.Title, Body: n.Body, Data: actionData(n.Action),
	})
	if err != nil {
		slog.Warn("notification not shown", "title", n.Title, "error", err)
	}
}

// OpenWindow shows the one app window on the web UI. Closing it hides it;
// the app keeps running in the menu bar.
func (s *shell) OpenWindow(url string) {
	slog.Debug("open window")
	application.InvokeAsync(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.window == nil {
			s.window = s.app.Window.NewWithOptions(application.WebviewWindowOptions{
				Name: "main", Title: "Ícaro", Width: 1280, Height: 860, URL: url,
			})
			w := s.window
			w.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
				w.Hide()
				e.Cancel()
			})
		} else {
			s.window.SetURL(url)
		}
		s.window.Show()
		s.window.Focus()
	})
}

func (s *shell) OpenBrowser(url string) {
	if err := s.app.Browser.OpenURL(url); err != nil {
		slog.Warn("could not open the browser", "url", url, "error", err)
	}
}

func (s *shell) Quit() { application.InvokeAsync(s.app.Quit) }

// actionData carries a notification's action through the notification
// center, so clicking it does what the matching menu item does.
func actionData(a desktop.Action) map[string]any {
	if a.Kind == desktop.ActNone {
		return nil
	}
	return map[string]any{"action": string(a.Kind), "arg": a.Arg}
}

func actionFromData(d map[string]any) desktop.Action {
	kind, _ := d["action"].(string)
	arg, _ := d["arg"].(string)
	// Only actions a notification can carry; nothing destructive.
	switch k := desktop.ActionKind(kind); k {
	case desktop.ActOpenUI, desktop.ActStartDaemon:
		return desktop.Action{Kind: k, Arg: arg}
	}
	return desktop.Action{Kind: desktop.ActOpenUI}
}

// loginItem is launch at login through Wails: a login item registered
// with SMAppService for a bundled app on macOS 13+, a LaunchAgent
// otherwise, and an XDG autostart entry on Linux.
type loginItem struct{ m *application.AutostartManager }

func newLoginItem(app *application.App) desktop.LoginItem {
	switch runtime.GOOS {
	case "darwin", "linux":
		return loginItem{app.Autostart}
	}
	return nil
}

func (l loginItem) Enabled() (bool, error) { return l.m.IsEnabled() }

func (l loginItem) SetEnabled(on bool) error {
	if on {
		return l.m.Enable()
	}
	return l.m.Disable()
}

// labels flattens a menu for the debug log.
func labels(items []desktop.Item) []string {
	var out []string
	for _, it := range items {
		switch {
		case it.Separator:
			out = append(out, "---")
		case len(it.Children) > 0:
			out = append(out, it.Label+" ▸ "+strings.Join(labels(it.Children), " | "))
		default:
			out = append(out, it.Label)
		}
	}
	return out
}
