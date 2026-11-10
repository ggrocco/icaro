package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/assist"
	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow/events"
)

// fakeChats records calls and answers with canned values. The API layer is
// a thin client of the assistant, so this is its edge.
type fakeChats struct {
	calls chan string
	err   error
	lines chan []assist.LogLine // what Entries returns next
}

func (f *fakeChats) rec(s string) error {
	select {
	case f.calls <- s:
	default:
	}
	return f.err
}

func (f *fakeChats) CreateChat(_ context.Context, req assist.CreateRequest, actor string) (*assist.ChatDetail, error) {
	if err := f.rec("create " + req.Workflow + " " + actor); err != nil {
		return nil, err
	}
	return &assist.ChatDetail{ChatInfo: assist.ChatInfo{ID: "c1", Workflow: req.Workflow}}, nil
}

func (f *fakeChats) ListChats(context.Context) ([]assist.ChatInfo, error) {
	return []assist.ChatInfo{{ID: "c1"}}, f.rec("list")
}

func (f *fakeChats) GetChat(_ context.Context, id string) (*assist.ChatDetail, error) {
	if err := f.rec("get " + id); err != nil {
		return nil, err
	}
	return &assist.ChatDetail{ChatInfo: assist.ChatInfo{ID: id}}, nil
}

func (f *fakeChats) Send(_ context.Context, id, text, actor string) ([]assist.MessageInfo, error) {
	return []assist.MessageInfo{{ID: "m1", Text: text}}, f.rec("send " + id + " " + text + " " + actor)
}

func (f *fakeChats) Retry(_ context.Context, id, msg, _ string) ([]assist.MessageInfo, error) {
	return []assist.MessageInfo{{ID: "m2"}}, f.rec("retry " + id + " " + msg)
}

func (f *fakeChats) Cancel(_ context.Context, id string) error { return f.rec("cancel " + id) }

func (f *fakeChats) PutDraft(_ context.Context, id, yaml string) (*assist.DraftInfo, error) {
	return &assist.DraftInfo{YAML: yaml, Version: 2}, f.rec("draft " + id)
}

func (f *fakeChats) Decide(_ context.Context, id, ap string, approve bool, actor string) (*assist.ApprovalInfo, error) {
	d := "declined"
	if approve {
		d = "approved"
	}
	return &assist.ApprovalInfo{ID: ap, Decision: d, Actor: actor}, f.rec("decide " + id + " " + ap + " " + d + " " + actor)
}

func (f *fakeChats) Delete(_ context.Context, id string) error { return f.rec("delete " + id) }

func (f *fakeChats) Entries(_ context.Context, _ string, offset int64) ([]assist.LogLine, int64, error) {
	if f.err != nil {
		return nil, offset, f.err
	}
	select {
	case ls := <-f.lines:
		if len(ls) == 0 {
			return nil, offset, nil
		}
		return ls, ls[len(ls)-1].Next, nil
	default:
		return nil, offset, nil
	}
}

type chatFixture struct {
	t      *testing.T
	srv    *httptest.Server
	chats  *fakeChats
	bus    *events.Bus
	write  string
	read   string
	cancel context.CancelFunc
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet}), quiet)
	f := &chatFixture{t: t, chats: &fakeChats{calls: make(chan string, 16), lines: make(chan []assist.LogLine, 16)}, bus: events.New(ctx), cancel: cancel}
	RegisterChats(s, f.chats, f.bus)
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	for _, tok := range []struct {
		name, scope string
		dst         *string
	}{{"writer", store.ScopeWrite, &f.write}, {"reader", store.ScopeRead, &f.read}} {
		plain, hash, _ := crypto.NewToken()
		if _, err := st.CreateToken(ctx, tok.name, hash, tok.scope); err != nil {
			t.Fatal(err)
		}
		*tok.dst = plain
	}
	return f
}

func (f *chatFixture) do(method, path, tok, body string) (int, string) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (f *chatFixture) call() string {
	f.t.Helper()
	select {
	case c := <-f.chats.calls:
		return c
	case <-time.After(5 * time.Second):
		f.t.Fatal("no call")
		return ""
	}
}

func TestChatRoutes(t *testing.T) {
	f := newChatFixture(t)
	for _, tc := range []struct {
		method, path, body string
		status             int
		call, contains     string
	}{
		{"GET", "/api/v1/chats", "", 200, "list", `"chats"`},
		{"POST", "/api/v1/chats", `{"workflow":"hello"}`, 201, "create hello token:writer", `"workflow": "hello"`},
		{"GET", "/api/v1/chats/c1", "", 200, "get c1", `"id": "c1"`},
		{"POST", "/api/v1/chats/c1/messages", `{"text":"hi"}`, 202, "send c1 hi token:writer", `"messages"`},
		{"POST", "/api/v1/chats/c1/messages/m1/retry", "", 202, "retry c1 m1", `"m2"`},
		{"POST", "/api/v1/chats/c1/cancel", "", 204, "cancel c1", ""},
		{"PUT", "/api/v1/chats/c1/draft", `{"yaml":"name: x"}`, 200, "draft c1", `"version": 2`},
		{"POST", "/api/v1/chats/c1/approvals/a1/approve", "", 200, "decide c1 a1 approved token:writer", `"approved"`},
		{"POST", "/api/v1/chats/c1/approvals/a1/decline", "", 200, "decide c1 a1 declined token:writer", `"declined"`},
		{"DELETE", "/api/v1/chats/c1", "", 204, "delete c1", ""},
	} {
		status, body := f.do(tc.method, tc.path, f.write, tc.body)
		if status != tc.status || !strings.Contains(body, tc.contains) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, status, body)
			continue
		}
		if got := f.call(); got != tc.call {
			t.Errorf("%s %s called %q, want %q", tc.method, tc.path, got, tc.call)
		}
	}
	// A read token can follow a chat but not talk to it.
	for _, p := range []string{"/api/v1/chats", "/api/v1/chats/c1/messages", "/api/v1/chats/c1/approvals/a1/approve"} {
		if status, _ := f.do("POST", p, f.read, `{}`); status != http.StatusForbidden {
			t.Errorf("read token POST %s: %d", p, status)
		}
	}
	// Malformed bodies are 400s.
	for _, p := range []string{"/api/v1/chats", "/api/v1/chats/c1/messages"} {
		if status, _ := f.do("POST", p, f.write, `{"nope":1}`); status != http.StatusBadRequest {
			t.Errorf("unknown field %s: %d", p, status)
		}
	}
	if status, _ := f.do("PUT", "/api/v1/chats/c1/draft", f.write, `[`); status != http.StatusBadRequest {
		t.Errorf("bad draft body: %d", status)
	}
}

func TestChatErrors(t *testing.T) {
	f := newChatFixture(t)
	for err, want := range map[error]int{
		assist.ErrNotFound:                     404,
		errors.Join(assist.ErrInvalid):         400,
		errors.Join(assist.ErrBusy):            409,
		errors.New("disk full"):                500,
		&service.Unavailable{Msg: "no docker"}: 503,
	} {
		f.chats.err = err
		for _, r := range [][3]string{
			{"GET", "/api/v1/chats", ""}, {"POST", "/api/v1/chats", "{}"}, {"GET", "/api/v1/chats/c1", ""},
			{"POST", "/api/v1/chats/c1/messages", `{"text":"x"}`}, {"POST", "/api/v1/chats/c1/messages/m/retry", ""},
			{"POST", "/api/v1/chats/c1/cancel", ""}, {"PUT", "/api/v1/chats/c1/draft", "{}"},
			{"POST", "/api/v1/chats/c1/approvals/a/approve", ""}, {"DELETE", "/api/v1/chats/c1", ""},
			{"GET", "/api/v1/chats/c1/events", ""},
		} {
			if status, body := f.do(r[0], r[1], f.write, r[2]); status != want {
				t.Errorf("%v: %s %s = %d %s", err, r[0], r[1], status, body)
			}
		}
	}
}

func TestChatDecideAnonymous(t *testing.T) {
	h := &chatHandlers{c: &fakeChats{calls: make(chan string, 1)}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)
	req.SetPathValue("id", "c1")
	req.SetPathValue("approval", "a1")
	h.decide(true)(rec, req)
	if !strings.Contains(rec.Body.String(), `"actor": "someone"`) {
		t.Fatal(rec.Body.String())
	}
}

func TestChatEvents(t *testing.T) {
	saved := sseKeepAlive
	sseKeepAlive = 20 * time.Millisecond
	t.Cleanup(func() { sseKeepAlive = saved })
	f := newChatFixture(t)
	f.chats.lines <- nil // the existence check
	f.chats.lines <- []assist.LogLine{{Data: json.RawMessage(`{"type":"message"}`), Next: 30}, {Data: json.RawMessage(`{"type":"status"}`), Next: 50}}
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/v1/chats/c1/events?offset=10", nil)
	req.Header.Set("Authorization", "Bearer "+f.read)
	req.Header.Set("Last-Event-ID", "12")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	r := bufio.NewReader(res.Body)
	readEvent := func() string {
		t.Helper()
		var b strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v (%q)", err, b.String())
			}
			if line == "\n" {
				if strings.HasPrefix(b.String(), ":") {
					b.Reset()
					continue // keep-alive
				}
				return b.String()
			}
			b.WriteString(line)
		}
	}
	if ev := readEvent(); ev != "event: entry\nid: 30\ndata: {\"type\":\"message\"}\n" {
		t.Fatalf("%q", ev)
	}
	if ev := readEvent(); !strings.Contains(ev, "id: 50") {
		t.Fatalf("%q", ev)
	}
	// The log grows and the bus rings: the new entry follows.
	time.Sleep(50 * time.Millisecond) // a keep-alive goes by
	f.chats.lines <- []assist.LogLine{{Data: json.RawMessage(`{"type":"event"}`), Next: 80}}
	f.bus.Publish(events.Event{Type: "chat", RunID: assist.Topic("c1"), Offset: 80})
	if ev := readEvent(); !strings.Contains(ev, "id: 80") || !strings.Contains(ev, `"event"`) {
		t.Fatalf("%q", ev)
	}
	// The bus closing tells the client to resync from where it was.
	f.cancel()
	if ev := readEvent(); ev != "event: resync\nid: 80\ndata: {}\n" {
		t.Fatalf("%q", ev)
	}
}
