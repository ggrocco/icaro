package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/paths"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/usage"
)

// fakeAPI is the slice of the daemon API the app uses.
type fakeAPI struct {
	mu        sync.Mutex
	down      bool
	runs      map[string][]*service.RunInfo // by status
	previews  []apiclient.Preview
	workflows []*service.WorkflowInfo
	usage     usage.Report
	fail      map[string]int // path prefix -> HTTP status
	calls     []string
	pings     int // health checks of the local daemon
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{runs: map[string][]*service.RunInfo{}, fail: map[string]int{}, usage: usage.Report{Timezone: "UTC"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) set(fn func(f *fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeAPI) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	for prefix, code := range f.fail {
		if strings.HasPrefix(r.Method+" "+r.URL.Path, prefix) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "injected " + prefix})
			return
		}
	}
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch p := r.URL.Path; {
	case p == "/healthz":
		reply(map[string]string{"status": "ok"})
	case r.Method == http.MethodGet && p == "/api/v1/runs":
		reply(map[string]any{"runs": f.runs[r.URL.Query().Get("status")]})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/cancel"):
		reply(service.RunInfo{ID: strings.Split(p, "/")[4], Status: store.RunCancelled})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/api/v1/workflows/") && strings.HasSuffix(p, "/runs"):
		reply(service.RunInfo{ID: "r-new", Workflow: strings.Split(p, "/")[4], Status: store.RunQueued})
	case p == "/api/v1/previews":
		reply(map[string]any{"previews": f.previews})
	case p == "/api/v1/workflows":
		reply(map[string]any{"workflows": f.workflows})
	case p == "/api/v1/usage":
		reply(f.usage)
	case r.Method == http.MethodPost && p == "/api/v1/ui/link":
		reply(map[string]any{"url": "http://127.0.0.1:8787/ui/?token=one-time"})
	default:
		http.NotFound(w, r)
	}
}

// remoteDaemon reaches the fake as a TCP daemon.
func remoteDaemon(srv *httptest.Server) *Daemon {
	return &Daemon{Client: apiclient.New(srv.URL, "t"), Remote: true}
}

// localDaemon reaches the fake as the local daemon, with its health
// check and start replaced.
type starter struct {
	mu    sync.Mutex
	calls [][]string
	err   error
	up    func() // brings the fake daemon up
}

func (s *starter) start(_ context.Context, _ paths.Layout, exe string, args []string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string{exe}, args...))
	if s.err == nil && s.up != nil {
		s.up()
	}
	return s.err
}

func (s *starter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func localDaemon(f *fakeAPI, srv *httptest.Server, st *starter) *Daemon {
	return &Daemon{
		Client: apiclient.New(srv.URL, ""), icaro: "/opt/icaro/bin/icaro", args: []string{"--home", "/h"},
		start: st.start,
		ping: func(context.Context, paths.Layout) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.pings++
			if f.down {
				return errors.New("dial unix icaro.sock: connect: no such file or directory")
			}
			return nil
		},
	}
}

// fakeUI records what the controller asks of the shell.
type fakeUI struct {
	mu      sync.Mutex
	menus   [][]Item
	titles  []string
	notes   []Notification
	windows []string
	browser []string
	quit    int
}

func (u *fakeUI) SetMenu(items []Item, title string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.menus, u.titles = append(u.menus, items), append(u.titles, title)
}

func (u *fakeUI) Notify(n Notification) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.notes = append(u.notes, n)
}

func (u *fakeUI) OpenWindow(url string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.windows = append(u.windows, url)
}

func (u *fakeUI) OpenBrowser(url string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.browser = append(u.browser, url)
}

func (u *fakeUI) Quit() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.quit++
}

func (u *fakeUI) snapshot() fakeUI {
	u.mu.Lock()
	defer u.mu.Unlock()
	return fakeUI{menus: append([][]Item(nil), u.menus...), titles: append([]string(nil), u.titles...),
		notes: append([]Notification(nil), u.notes...), windows: append([]string(nil), u.windows...),
		browser: append([]string(nil), u.browser...), quit: u.quit}
}

func (u *fakeUI) lastMenu() []Item {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.menus) == 0 {
		return nil
	}
	return u.menus[len(u.menus)-1]
}

// labels flattens a menu for assertions.
func labels(items []Item) string {
	var out []string
	for _, it := range items {
		switch {
		case it.Separator:
			out = append(out, "---")
		case len(it.Children) > 0:
			out = append(out, it.Label+"{"+labels(it.Children)+"}")
		default:
			out = append(out, it.Label)
		}
	}
	return strings.Join(out, "; ")
}

// find returns the item with a label, searching submenus.
func find(items []Item, label string) (Item, bool) {
	for _, it := range items {
		if it.Label == label {
			return it, true
		}
		if c, ok := find(it.Children, label); ok {
			return c, true
		}
	}
	return Item{}, false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
