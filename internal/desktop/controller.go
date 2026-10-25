package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

// UI is what the shell provides. Calls may come from any goroutine; the
// shell moves them to its UI thread.
type UI interface {
	SetMenu(items []Item, title string)
	Notify(n Notification)
	// OpenWindow shows the app's window on the web UI at url.
	OpenWindow(url string)
	// OpenBrowser opens a preview in the default browser.
	OpenBrowser(url string)
	Quit()
}

// LoginItem is the platform's launch-at-login registration.
type LoginItem interface {
	Enabled() (bool, error)
	SetEnabled(on bool) error
}

// Controller runs the app: it polls the daemon, renders the menu, posts
// notifications and carries out actions.
type Controller struct {
	Daemon *Daemon
	UI     UI
	// Login is nil where launch at login is not supported.
	Login     LoginItem
	PrefsPath string
	// ForceTrayOnly opens no window at launch whatever the settings say
	// (the --tray-only flag).
	ForceTrayOnly bool
	Interval      time.Duration // default 3s
	UsageInterval time.Duration // default 1m
	Now           func() time.Time

	mu        sync.Mutex
	prefs     StoredPrefs
	state     State
	watcher   Watcher
	usage     *Usage
	usageAt   time.Time
	lastMenu  []Item
	lastTitle string
	poke      chan struct{}
}

func (c *Controller) init() {
	if c.Interval <= 0 {
		c.Interval = 3 * time.Second
	}
	if c.UsageInterval <= 0 {
		c.UsageInterval = time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	c.watcher.Misses = 2
	c.poke = make(chan struct{}, 1)
}

// Run attaches to (or starts) the daemon, opens the window unless in
// tray-only mode, and polls until ctx ends. It returns ctx's error.
func (c *Controller) Run(ctx context.Context) error {
	c.init()
	prefs, err := LoadPrefs(c.PrefsPath)
	if err != nil {
		c.UI.Notify(Notification{ID: "prefs", Title: "Could not read the app settings", Body: err.Error()})
	}
	c.mu.Lock()
	c.prefs = prefs
	c.mu.Unlock()
	c.render()

	started := c.Daemon.Ensure(ctx)
	if started != nil {
		// Say why; the outage notice would only repeat the failed health
		// check, so it is not posted on top of this one.
		c.fail("Could not start the Ícaro daemon", started)
		c.mu.Lock()
		c.watcher.down = true
		c.mu.Unlock()
	}
	c.refresh(ctx)
	if started == nil && !c.TrayOnly() {
		if err := c.Do(ctx, Action{Kind: ActOpenUI}); err != nil {
			c.fail("Could not open Ícaro", err)
		}
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-c.poke:
		}
		c.refresh(ctx)
	}
}

// TrayOnly reports whether launches open no window.
func (c *Controller) TrayOnly() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ForceTrayOnly || c.prefs.TrayOnly
}

// State returns the last poll.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Controller) refresh(ctx context.Context) {
	s := Fetch(ctx, c.Daemon)
	now := c.Now()
	c.mu.Lock()
	fetchUsage := s.Reachable && (c.usage == nil || now.Sub(c.usageAt) >= c.UsageInterval)
	c.mu.Unlock()
	var u *Usage
	if fetchUsage {
		u = FetchUsage(ctx, c.Daemon, now)
	}
	c.mu.Lock()
	switch {
	case fetchUsage:
		c.usage, c.usageAt = u, now
	case !s.Reachable:
		// Refetch as soon as the daemon is back.
		c.usage = nil
	}
	if s.Reachable {
		s.Usage = c.usage
	}
	c.state = s
	notes := c.watcher.Observe(s)
	c.mu.Unlock()
	for _, n := range notes {
		c.UI.Notify(n)
	}
	c.render()
}

// render pushes the menu to the shell when it changed, so an open menu
// is not rebuilt under the pointer every poll.
func (c *Controller) render() {
	c.mu.Lock()
	p := Prefs{TrayOnly: c.ForceTrayOnly || c.prefs.TrayOnly}
	c.mu.Unlock()
	if c.Login != nil {
		if on, err := c.Login.Enabled(); err == nil {
			p.LoginItem = &on
		}
	}
	c.mu.Lock()
	items, title := Menu(c.state, p), Title(c.state)
	changed := title != c.lastTitle || !reflect.DeepEqual(items, c.lastMenu)
	c.lastMenu, c.lastTitle = items, title
	c.mu.Unlock()
	if changed {
		c.UI.SetMenu(items, title)
	}
}

// Act runs an action in the background, as menu clicks do, and reports a
// failure as a notification.
func (c *Controller) Act(ctx context.Context, a Action) {
	go func() {
		if err := c.Do(ctx, a); err != nil {
			c.fail(failTitle(a.Kind), err)
		}
	}()
}

func failTitle(k ActionKind) string {
	switch k {
	case ActOpenUI:
		return "Could not open Ícaro"
	case ActOpenURL:
		return "Could not open the preview"
	case ActCancelRun:
		return "Could not cancel the run"
	case ActRunWorkflow:
		return "Could not start the workflow"
	case ActStartDaemon:
		return "Could not start the Ícaro daemon"
	case ActLoginItem:
		return "Could not change launch at login"
	case ActTrayOnly:
		return "Could not save the setting"
	}
	return "Ícaro: " + string(k) + " failed"
}

func (c *Controller) fail(title string, err error) {
	c.UI.Notify(Notification{ID: "error", Title: title, Body: clip(err.Error(), 200)})
}

// Do carries out an action.
func (c *Controller) Do(ctx context.Context, a Action) error {
	defer c.refreshSoon()
	switch a.Kind {
	case ActOpenUI:
		u, err := c.uiURL(ctx, a.Arg)
		if err != nil {
			return err
		}
		c.UI.OpenWindow(u)
	case ActOpenURL:
		u, err := url.Parse(a.Arg)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("not a web URL: %q", a.Arg)
		}
		c.UI.OpenBrowser(a.Arg)
	case ActCancelRun:
		_, err := c.Daemon.Client.CancelRun(ctx, a.Arg)
		return err
	case ActRunWorkflow:
		r, err := c.Daemon.Client.Run(ctx, a.Arg, nil)
		if err != nil {
			return err
		}
		c.UI.Notify(Notification{ID: "run-started-" + r.ID, Title: "Started " + a.Arg, Body: "Run " + r.ID,
			Action: Action{Kind: ActOpenUI, Arg: "runs/" + url.PathEscape(r.ID)}})
	case ActStartDaemon:
		return c.Daemon.Ensure(ctx)
	case ActLoginItem:
		if c.Login == nil {
			return errors.New("launch at login is not supported here")
		}
		on, err := c.Login.Enabled()
		if err != nil {
			return err
		}
		if err := c.Login.SetEnabled(!on); err != nil {
			return err
		}
		c.render()
	case ActTrayOnly:
		c.mu.Lock()
		p := c.prefs
		p.TrayOnly = !p.TrayOnly
		c.mu.Unlock()
		if err := SavePrefs(c.PrefsPath, p); err != nil {
			return err
		}
		c.mu.Lock()
		c.prefs = p
		c.mu.Unlock()
		c.render()
	case ActQuit:
		// The daemon keeps running: it owns the runs, not the app.
		c.UI.Quit()
	default:
		return fmt.Errorf("unknown action %q", a.Kind)
	}
	return nil
}

func (c *Controller) refreshSoon() {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

// uiURL is a signed-in URL of the web UI at route. A local daemon mints a
// one-time sign-in link over its socket, as `icaro ui` does; a TCP daemon
// cannot, so its UI asks for a token.
func (c *Controller) uiURL(ctx context.Context, route string) (string, error) {
	frag := ""
	if route != "" {
		frag = "#/" + route
	}
	if c.Daemon.Remote {
		return strings.TrimRight(c.Daemon.Client.BaseURL, "/") + "/ui/" + frag, nil
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.Daemon.Client.Do(ctx, http.MethodPost, "/api/v1/ui/link", nil, &out); err != nil {
		return "", err
	}
	return out.URL + frag, nil
}
