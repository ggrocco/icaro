package desktop

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"icaro/internal/paths"
	"icaro/internal/service"
	"icaro/internal/store"
)

type fakeLogin struct {
	mu  sync.Mutex
	on  bool
	err error
}

func (l *fakeLogin) Enabled() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.on, l.err
}

func (l *fakeLogin) SetEnabled(on bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.on = on
	return nil
}

type rig struct {
	api  *fakeAPI
	st   *starter
	ui   *fakeUI
	ctrl *Controller
	done chan error
	stop context.CancelFunc
}

func newRig(t *testing.T, configure func(r *rig)) *rig {
	t.Helper()
	f, srv := newFakeAPI(t)
	r := &rig{api: f, st: &starter{}, ui: &fakeUI{}}
	r.ctrl = &Controller{
		Daemon: localDaemon(f, srv, r.st), UI: r.ui, PrefsPath: filepath.Join(t.TempDir(), "desktop.json"),
		Interval: 5 * time.Millisecond, UsageInterval: time.Hour,
	}
	if configure != nil {
		configure(r)
	}
	return r
}

func (r *rig) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.stop, r.done = cancel, make(chan error, 1)
	go func() { r.done <- r.ctrl.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-r.done; !errors.Is(err, context.Canceled) {
			t.Errorf("run: %v", err)
		}
	})
}

// polls waits for n more health checks: n more polls of the daemon.
func (r *rig) polls(t *testing.T, n int) {
	t.Helper()
	count := func() int {
		r.api.mu.Lock()
		defer r.api.mu.Unlock()
		return r.api.pings
	}
	start := count()
	waitFor(t, "polls", func() bool { return count() >= start+n })
}

func TestControllerLaunchOpensTheUI(t *testing.T) {
	r := newRig(t, nil)
	r.run(t)
	waitFor(t, "window", func() bool { return len(r.ui.snapshot().windows) == 1 })
	if w := r.ui.snapshot().windows[0]; w != "http://127.0.0.1:8787/ui/?token=one-time" {
		t.Fatal(w)
	}
	if r.st.count() != 0 {
		t.Fatal("a running daemon is not started again")
	}
	r.polls(t, 3)
	ui := r.ui.snapshot()
	if !strings.HasPrefix(labels(ui.menus[len(ui.menus)-1]), "Ícaro is running") {
		t.Fatal(labels(ui.menus[len(ui.menus)-1]))
	}
	// Identical polls do not rebuild the menu.
	if len(ui.menus) > 3 {
		t.Fatalf("%d menus for an unchanged state", len(ui.menus))
	}
	if r.ctrl.State().Usage == nil || len(ui.notes) != 0 {
		t.Fatalf("%+v %+v", r.ctrl.State(), ui.notes)
	}
}

func TestControllerTrayOnly(t *testing.T) {
	for name, configure := range map[string]func(r *rig){
		"flag": func(r *rig) { r.ctrl.ForceTrayOnly = true },
		"setting": func(r *rig) {
			if err := SavePrefs(r.ctrl.PrefsPath, StoredPrefs{TrayOnly: true}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, configure)
			r.run(t)
			r.polls(t, 3)
			if w := r.ui.snapshot().windows; len(w) != 0 || !r.ctrl.TrayOnly() {
				t.Fatalf("tray-only opens no window: %v", w)
			}
			if it, _ := find(r.ui.lastMenu(), "Tray only (no window at launch)"); !it.Checked {
				t.Fatal("checked")
			}
		})
	}
}

func TestControllerStartsAMissingDaemon(t *testing.T) {
	r := newRig(t, func(r *rig) {
		r.api.down = true
		r.st.up = func() { r.api.down = false }
	})
	r.run(t)
	waitFor(t, "window", func() bool { return len(r.ui.snapshot().windows) == 1 })
	if r.st.count() != 1 {
		t.Fatal(r.st.count())
	}
	if ui := r.ui.snapshot(); labels(ui.menus[0])[:20] != "Daemon not reachable" {
		t.Fatalf("the menu is up before the daemon: %s", labels(ui.menus[0]))
	}
}

func TestControllerNotifiesOutagesAndFailures(t *testing.T) {
	r := newRig(t, func(r *rig) {
		r.api.down = true
		r.st.err = errors.New("daemon exited during startup; see daemon.log")
	})
	r.run(t)
	waitFor(t, "start failure", func() bool { return len(r.ui.snapshot().notes) == 1 })
	if n := r.ui.snapshot().notes[0]; n.Title != "Could not start the Ícaro daemon" || !strings.Contains(n.Body, "daemon.log") {
		t.Fatalf("the reason is shown: %+v", n)
	}
	r.polls(t, 3)
	if n := r.ui.snapshot().notes; len(n) != 1 {
		t.Fatalf("no outage notice on top of the start failure: %+v", n)
	}
	if len(r.ui.snapshot().windows) != 0 {
		t.Fatal("no window without a daemon")
	}
	if it, ok := find(r.ui.lastMenu(), "Start daemon"); !ok || it.Action.Kind != ActStartDaemon {
		t.Fatal(labels(r.ui.lastMenu()))
	}

	r.api.set(func(f *fakeAPI) { f.down = false })
	waitFor(t, "back", func() bool { return len(r.ui.snapshot().notes) == 2 })
	r.polls(t, 2)
	r.api.set(func(f *fakeAPI) {
		f.runs[store.RunFailed] = []*service.RunInfo{{ID: "r1", Workflow: "deploy", Status: store.RunFailed, Error: "exit 1"}}
	})
	waitFor(t, "failure", func() bool { return len(r.ui.snapshot().notes) == 3 })
	if n := r.ui.snapshot().notes[2]; n.Title != "Run failed: deploy" || n.Action.Arg != "runs/r1" {
		t.Fatalf("%+v", n)
	}
	if r.ui.snapshot().titles[0] != "!" {
		t.Fatal("unreachable is marked in the tray")
	}
}

func TestControllerUsageCadence(t *testing.T) {
	r := newRig(t, func(r *rig) { r.ctrl.ForceTrayOnly = true })
	r.run(t)
	r.polls(t, 4)
	usageCalls := func() int { return strings.Count(strings.Join(r.api.called(), "\n"), "/api/v1/usage") }
	if usageCalls() != 1 {
		t.Fatalf("usage is polled less often: %d", usageCalls())
	}
	r.api.set(func(f *fakeAPI) { f.down = true })
	waitFor(t, "down", func() bool { return !r.ctrl.State().Reachable })
	r.api.set(func(f *fakeAPI) { f.down = false })
	waitFor(t, "usage refetched after the outage", func() bool { return usageCalls() == 2 })
}

func TestControllerPrefsUnreadable(t *testing.T) {
	r := newRig(t, func(r *rig) {
		_ = os.WriteFile(r.ctrl.PrefsPath, []byte("{"), 0o600)
		r.ctrl.ForceTrayOnly = true
	})
	r.run(t)
	waitFor(t, "warning", func() bool { return len(r.ui.snapshot().notes) == 1 })
	if n := r.ui.snapshot().notes[0]; n.ID != "prefs" {
		t.Fatalf("%+v", n)
	}
}

func TestControllerActions(t *testing.T) {
	ctx := context.Background()
	login := &fakeLogin{}
	r := newRig(t, func(r *rig) { r.ctrl.Login = login })
	r.ctrl.init()
	c, ui := r.ctrl, r.ui

	if err := c.Do(ctx, Action{Kind: ActOpenUI, Arg: "runs/r1"}); err != nil {
		t.Fatal(err)
	}
	if w := ui.snapshot().windows; w[0] != "http://127.0.0.1:8787/ui/?token=one-time#/runs/r1" {
		t.Fatal(w)
	}
	if err := c.Do(ctx, Action{Kind: ActOpenURL, Arg: "http://feat-x.localhost"}); err != nil || ui.snapshot().browser[0] != "http://feat-x.localhost" {
		t.Fatal(err)
	}
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "%zz"} {
		if err := c.Do(ctx, Action{Kind: ActOpenURL, Arg: bad}); err == nil {
			t.Errorf("%q must not open", bad)
		}
	}
	if err := c.Do(ctx, Action{Kind: ActCancelRun, Arg: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, Action{Kind: ActRunWorkflow, Arg: "fix-ci"}); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.api.called(), "\n")
	if !strings.Contains(calls, "POST /api/v1/runs/r1/cancel") || !strings.Contains(calls, "POST /api/v1/workflows/fix-ci/runs") {
		t.Fatal(calls)
	}
	if n := ui.snapshot().notes; n[0].Title != "Started fix-ci" || n[0].Action.Arg != "runs/r-new" {
		t.Fatalf("%+v", n)
	}
	if err := c.Do(ctx, Action{Kind: ActStartDaemon}); err != nil || r.st.count() != 0 {
		t.Fatal(err)
	}

	// Launch at login toggles the platform registration and re-renders.
	if err := c.Do(ctx, Action{Kind: ActLoginItem}); err != nil || !login.on {
		t.Fatal(err)
	}
	if it, _ := find(ui.lastMenu(), "Launch at login"); !it.Checked {
		t.Fatal(labels(ui.lastMenu()))
	}
	login.err = errors.New("SMAppService not registered")
	if err := c.Do(ctx, Action{Kind: ActLoginItem}); err == nil {
		t.Fatal("status errors are returned")
	}
	c.render()
	if _, ok := find(ui.lastMenu(), "Launch at login"); ok {
		t.Fatal("an unknown login state is not shown as off")
	}

	// Tray-only is saved.
	if err := c.Do(ctx, Action{Kind: ActTrayOnly}); err != nil {
		t.Fatal(err)
	}
	if p, _ := LoadPrefs(c.PrefsPath); !p.TrayOnly || !c.TrayOnly() {
		t.Fatal("saved")
	}

	if err := c.Do(ctx, Action{Kind: ActQuit}); err != nil || ui.snapshot().quit != 1 {
		t.Fatal(err)
	}
	if err := c.Do(ctx, Action{Kind: "nope"}); err == nil {
		t.Fatal("unknown action")
	}

	// Failures.
	r.api.set(func(f *fakeAPI) {
		f.fail["POST /api/v1/workflows/"] = http.StatusConflict
		f.fail["POST /api/v1/ui/link"] = http.StatusForbidden
	})
	if err := c.Do(ctx, Action{Kind: ActRunWorkflow, Arg: "busy"}); err == nil {
		t.Fatal("run errors are returned")
	}
	if err := c.Do(ctx, Action{Kind: ActOpenUI}); err == nil {
		t.Fatal("link errors are returned")
	}
	before := len(ui.snapshot().notes)
	c.Act(ctx, Action{Kind: ActRunWorkflow, Arg: "busy"})
	waitFor(t, "error notification", func() bool { return len(ui.snapshot().notes) == before+1 })
	if n := ui.snapshot().notes[before]; n.ID != "error" || n.Title != "Could not start the workflow" || !strings.Contains(n.Body, "injected") {
		t.Fatalf("%+v", n)
	}
	for k, want := range map[ActionKind]string{ActOpenUI: "Could not open Ícaro", ActOpenURL: "Could not open the preview", ActCancelRun: "Could not cancel the run",
		ActStartDaemon: "Could not start the Ícaro daemon", ActLoginItem: "Could not change launch at login", ActTrayOnly: "Could not save the setting", "x": "Ícaro: x failed"} {
		if got := failTitle(k); got != want {
			t.Errorf("%s: %q", k, got)
		}
	}

	c.Login = nil
	if err := c.Do(ctx, Action{Kind: ActLoginItem}); err == nil {
		t.Fatal("unsupported")
	}
	c.PrefsPath = filepath.Join(c.PrefsPath, "under-a-file", "desktop.json")
	if err := c.Do(ctx, Action{Kind: ActTrayOnly}); err == nil {
		t.Fatal("save errors are returned")
	}
}

// A TCP daemon's UI cannot be signed into over the socket; the window
// opens its sign-in page.
func TestControllerRemoteUI(t *testing.T) {
	_, srv := newFakeAPI(t)
	ui := &fakeUI{}
	c := &Controller{Daemon: remoteDaemon(srv), UI: ui}
	if err := c.Do(context.Background(), Action{Kind: ActOpenUI, Arg: "usage"}); err != nil {
		t.Fatal(err)
	}
	if w := ui.snapshot().windows[0]; w != srv.URL+"/ui/#/usage" {
		t.Fatal(w)
	}
}

func TestPrefs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "desktop.json")
	if got, err := LoadPrefs(p); err != nil || got.TrayOnly {
		t.Fatal("missing is the defaults", err)
	}
	if err := SavePrefs(p, StoredPrefs{TrayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	if got, err := LoadPrefs(p); err != nil || !got.TrayOnly {
		t.Fatal(got, err)
	}
	if _, err := LoadPrefs(dir); err == nil {
		t.Fatal("a directory is unreadable")
	}
	if err := SavePrefs(filepath.Join(p, "x"), StoredPrefs{}); err == nil {
		t.Fatal("mkdir under a file")
	}
	if err := os.MkdirAll(filepath.Join(dir, "d", "desktop.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SavePrefs(filepath.Join(dir, "d", "desktop.json"), StoredPrefs{}); err == nil {
		t.Fatal("write error")
	}
	if PrefsPath(r0()) != "/s/config/desktop.json" {
		t.Fatal(PrefsPath(r0()))
	}
}

func r0() paths.Layout { return paths.New("/s") }
