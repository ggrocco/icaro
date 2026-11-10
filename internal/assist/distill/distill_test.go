package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container"
	"icaro/internal/container/docker"
)

func TestCatalog(t *testing.T) {
	for _, m := range Catalog {
		if len(m.SHA256) != 64 || m.Size <= 0 || m.License != "apache-2.0" || !strings.HasSuffix(m.File, ".gguf") {
			t.Errorf("catalog entry %+v", m)
		}
	}
	if m, err := Lookup("qwen3.5-2b"); err != nil || m.Tier != TierCPU {
		t.Fatal(m, err)
	}
	if _, err := Lookup("gpt-5"); !errors.Is(err, ErrUnknownModel) || !strings.Contains(err.Error(), "qwen3.5-2b") {
		t.Fatal(err)
	}
	if AutoTier(4*gib) != TierMinimal || AutoTier(16*gib) != TierCPU || AutoTier(0) != TierCPU {
		t.Fatal("tiers")
	}
	if Default(TierCPU).Name != "qwen3.5-2b" || Default(TierMinimal).Name != "qwen3.5-0.8b" || Default("nope").Name != Catalog[0].Name {
		t.Fatal("defaults")
	}
}

// fakeInstall creates a sparse file of the model's size.
func fakeInstall(t *testing.T, s Store, m Model) {
	t.Helper()
	f, err := os.Create(s.Path(m))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(m.Size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestStorePick(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if _, ok, err := s.Pick("auto", TierCPU); ok || err != nil {
		t.Fatal("nothing installed")
	}
	small, _ := Lookup("qwen3.5-0.8b")
	cpu, _ := Lookup("qwen3.5-2b")
	fakeInstall(t, s, small)
	if m, ok, _ := s.Pick("", TierCPU); !ok || m.Name != small.Name {
		t.Fatalf("fallback to any installed: %+v", m)
	}
	fakeInstall(t, s, cpu)
	if got := s.Installed(); len(got) != 2 || got[0].Name != small.Name {
		t.Fatalf("installed %+v", got)
	}
	if m, ok, _ := s.Pick("auto", TierCPU); !ok || m.Name != cpu.Name {
		t.Fatalf("tier pick %+v", m)
	}
	if m, ok, _ := s.Pick("qwen3.5-0.8b", TierCPU); !ok || m.Name != small.Name {
		t.Fatalf("named pick %+v", m)
	}
	if _, ok, err := s.Pick("qwen3.5-9b", TierCPU); ok || err != nil {
		t.Fatal("a named model that is not installed")
	}
	if _, _, err := s.Pick("nope", TierCPU); err == nil {
		t.Fatal("unknown name")
	}
	// A partial file (wrong size) does not count.
	_ = os.Truncate(s.Path(cpu), 10)
	if len(s.Installed()) != 1 {
		t.Fatal("a partial file counts as installed")
	}
	if err := s.Remove(small); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(small); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatal(err)
	}
}

func TestPull(t *testing.T) {
	content := []byte("GGUF fake model bytes")
	sum := sha256.Sum256(content)
	m := Model{Name: "tiny", Repo: "org/tiny-GGUF", File: "tiny.gguf", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))}
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "evil") {
			_, _ = w.Write([]byte("something else entirely"))
			return
		}
		_, _ = w.Write(content)
	}))
	defer srv.Close()
	s := Store{Dir: filepath.Join(t.TempDir(), "models"), BaseURL: srv.URL}
	var last int64
	if err := s.Pull(context.Background(), m, func(done, _ int64) { last = done }); err != nil {
		t.Fatal(err)
	}
	if path != "/org/tiny-GGUF/resolve/main/tiny.gguf" || last != m.Size {
		t.Fatalf("path %s progress %d", path, last)
	}
	if b, _ := os.ReadFile(s.Path(m)); string(b) != string(content) {
		t.Fatal("content")
	}
	// Already in place: verified and kept.
	if err := s.Pull(context.Background(), m, nil); err != nil {
		t.Fatal(err)
	}
	// A tampered file in place fails verification.
	_ = os.WriteFile(s.Path(m), []byte(strings.Repeat("x", len(content))), 0o600)
	if err := s.Pull(context.Background(), m, nil); !errors.Is(err, ErrHashMismatch) {
		t.Fatal(err)
	}
	// A download that does not match is discarded.
	evil := m
	evil.Repo, evil.File = "org/evil", "evil.gguf"
	evil.Size = int64(len("something else entirely"))
	if err := s.Pull(context.Background(), evil, nil); !errors.Is(err, ErrHashMismatch) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path(evil)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a mismatched download was kept")
	}
	if _, err := os.Stat(s.Path(evil) + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a partial download was kept")
	}
	missing := m
	missing.Repo, missing.File = "org/missing", "missing.gguf"
	if err := s.Pull(context.Background(), missing, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal(err)
	}
	if err := (Store{Dir: s.Dir, BaseURL: "http://127.0.0.1:1"}).Pull(context.Background(), missing, nil); err == nil {
		t.Fatal("unreachable")
	}
	if err := (Store{Dir: s.Dir, BaseURL: "http://[::1"}).Pull(context.Background(), missing, nil); err == nil {
		t.Fatal("bad URL")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	if err := (Store{Dir: blocked}).Pull(context.Background(), m, nil); err == nil {
		t.Fatal("models dir under a file")
	}
	if err := (Store{Dir: t.TempDir()}).Verify(m); err == nil {
		t.Fatal("verify a missing file")
	}
}

// fakeServer answers like llama-server.
func fakeServer(t *testing.T, answer func(req chatRequest) (int, string)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`) })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		code, body := answer(req)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
	return mux
}

func content(s string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": s}}}})
	return string(b)
}

func TestClient(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(fakeServer(t, func(req chatRequest) (int, string) {
		got = req
		switch req.Messages[1].Content {
		case "empty":
			return 200, content("  ")
		case "broken":
			return 200, "not json"
		case "fail":
			return 500, strings.Repeat("e", 400)
		}
		return 200, content(`{"intent":"x"}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/", Model: "qwen"}
	ctx := context.Background()
	if err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := c.Complete(ctx, "sys", "hi", SummarySchema, 50)
	if err != nil || out != `{"intent":"x"}` {
		t.Fatal(out, err)
	}
	if got.Model != "qwen" || got.ResponseFormat.Type != "json_schema" || got.ChatTemplateKwargs["enable_thinking"] != false || got.Think == nil || *got.Think ||
		got.MaxTokens != 50 || got.Messages[0].Content != "sys" || !json.Valid(got.ResponseFormat.JSONSchema.Schema) {
		t.Fatalf("request %+v", got)
	}
	if _, err := c.Complete(ctx, "s", "empty", SummarySchema, 1); !errors.Is(err, ErrNoContent) {
		t.Fatal(err)
	}
	if _, err := c.Complete(ctx, "s", "broken", SummarySchema, 1); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatal(err)
	}
	if _, err := c.Complete(ctx, "s", "fail", SummarySchema, 1); err == nil || !strings.Contains(err.Error(), "500") || len(err.Error()) > 400 {
		t.Fatal(err)
	}
	down := &Client{BaseURL: "http://127.0.0.1:1"}
	if err := down.Ready(ctx); err == nil {
		t.Fatal("down")
	}
	if _, err := down.Complete(ctx, "s", "u", SummarySchema, 1); err == nil {
		t.Fatal("down")
	}
	if _, err := (&Client{BaseURL: "http://[::1"}).Complete(ctx, "s", "u", SummarySchema, 1); err == nil {
		t.Fatal("bad URL")
	}
	if err := (&Client{BaseURL: "http://[::1"}).Ready(ctx); err == nil {
		t.Fatal("bad URL")
	}
	// An Ollama-like server: no /health, but /v1/models.
	ollama := http.NewServeMux()
	ollama.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"data":[]}`) })
	o := httptest.NewServer(ollama)
	defer o.Close()
	if err := (&Client{BaseURL: o.URL}).Ready(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := down.WaitReady(cctx, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatal(err)
	}
	if err := c.WaitReady(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func serveUnix(t *testing.T, sock string, h http.Handler) func() {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "dst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestUnixClient(t *testing.T) {
	sock := filepath.Join(shortDir(t), "llama.sock")
	stop := serveUnix(t, sock, fakeServer(t, func(chatRequest) (int, string) { return 200, content(`{}`) }))
	defer stop()
	c := UnixClient(sock)
	if err := c.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out, err := c.Complete(context.Background(), "s", "u", SummarySchema, 1); err != nil || out != "{}" {
		t.Fatal(out, err)
	}
}

func TestParseSummary(t *testing.T) {
	s, err := ParseSummary([]byte(`{"intent":"  Post bugs\n to   #ops ","constraints":["weekdays","Weekdays","", "no paging"],
		"preferences":[],"rejected":[{"what":"page on-call","why":"too noisy"},{"what":"","why":"x"}],"open":["which repo?"],"resume":"Next: pick the repo."}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Intent != "Post bugs to #ops" || len(s.Constraints) != 2 || s.Constraints[1] != "no paging" || len(s.Rejected) != 1 || s.Empty() {
		t.Fatalf("%+v", s)
	}
	long := strings.Repeat("ü", maxResume+50)
	many := make([]string, 20)
	for i := range many {
		many[i] = strings.Repeat("c", i+1)
	}
	raw, _ := json.Marshal(map[string]any{"intent": long, "resume": long, "constraints": many, "preferences": nil, "rejected": nil, "open": nil})
	s, err = ParseSummary(raw)
	if err != nil || len([]rune(s.Intent)) != maxSentence || len([]rune(s.Resume)) != maxResume || len(s.Constraints) != maxItems {
		t.Fatalf("caps: %d %d %d %v", len([]rune(s.Intent)), len([]rune(s.Resume)), len(s.Constraints), err)
	}
	for _, bad := range []string{"", "[]", `{"intent":1}`, `{"extra":"field"}`, "not json"} {
		if _, err := ParseSummary([]byte(bad)); !errors.Is(err, ErrInvalidSummary) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if !(Summary{}).Empty() {
		t.Fatal("empty")
	}
}

func TestChunkAndRender(t *testing.T) {
	turns := []Turn{{"user", "aaaa"}, {"assistant", "bbbb"}, {"user", "cccc"}}
	if got := Chunk(turns, 9); len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 1 {
		t.Fatalf("%v", got)
	}
	if got := Chunk(turns, 100); len(got) != 1 {
		t.Fatalf("%v", got)
	}
	// A turn over the limit is split on rune boundaries.
	big := []Turn{{"user", "x"}, {"assistant", strings.Repeat("é", 7)}}
	parts := Chunk(big, 5)
	total := ""
	for _, p := range parts {
		for _, tt := range p {
			if len(tt.Text) > 5 {
				t.Fatalf("part over the limit: %q", tt.Text)
			}
			if tt.Role == "assistant" {
				total += tt.Text
			}
		}
	}
	if total != strings.Repeat("é", 7) {
		t.Fatalf("lost text: %q", total)
	}
	if Chunk(nil, 5) != nil {
		t.Fatal("empty")
	}
	if r := Render(turns[:2]); r != "PERSON: aaaa\n\nASSISTANT: bbbb\n\n" {
		t.Fatalf("%q", r)
	}
}

type fakeCompleter struct {
	calls []string
	out   []string
	err   error
}

func (f *fakeCompleter) Complete(_ context.Context, system, user string, _ json.RawMessage, _ int) (string, error) {
	f.calls = append(f.calls, system+"|"+user)
	if f.err != nil {
		return "", f.err
	}
	out := f.out[0]
	if len(f.out) > 1 {
		f.out = f.out[1:]
	}
	return out, nil
}

func TestSummarize(t *testing.T) {
	ctx := context.Background()
	full := `{"intent":"i","constraints":[],"preferences":[],"rejected":[],"open":[],"resume":"r"}`
	f := &fakeCompleter{out: []string{full}}
	s, u, err := Summarize(ctx, f, "hello", []Turn{{"user", "hi"}})
	if err != nil || s.Intent != "i" || u.Calls != 1 || u.Parts != 1 || !strings.Contains(f.calls[0], "about the workflow hello") {
		t.Fatal(s, u, err, f.calls)
	}
	// A long chat: one call per part, then a merge.
	long := []Turn{{"user", strings.Repeat("a", ChunkChars)}, {"assistant", strings.Repeat("b", ChunkChars)}}
	f = &fakeCompleter{out: []string{full}}
	_, u, err = Summarize(ctx, f, "", long)
	if err != nil || u.Parts != 2 || u.Calls != 3 || !strings.HasPrefix(f.calls[2], mergePrompt) || strings.Contains(f.calls[0], "about the workflow") {
		t.Fatalf("%+v %v", u, err)
	}
	if s, u, err := Summarize(ctx, f, "", nil); err != nil || !s.Empty() || u.Calls != 0 {
		t.Fatal("nothing to summarize")
	}
	boom := errors.New("boom")
	if _, _, err := Summarize(ctx, &fakeCompleter{err: boom}, "", long); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, _, err := Summarize(ctx, &fakeCompleter{out: []string{"nope"}}, "", long); !errors.Is(err, ErrInvalidSummary) {
		t.Fatal(err)
	}
	// The merge itself failing.
	merge := &countingCompleter{fail: 3, out: full}
	if _, _, err := Summarize(ctx, merge, "", long); err == nil {
		t.Fatal("merge error")
	}
}

type countingCompleter struct {
	n, fail int
	out     string
}

func (c *countingCompleter) Complete(context.Context, string, string, json.RawMessage, int) (string, error) {
	c.n++
	if c.n == c.fail {
		return "", errors.New("merge failed")
	}
	return c.out, nil
}

func TestManaged(t *testing.T) {
	dir := shortDir(t)
	fake := docker.NewFake()
	stops := make(chan func(), 2)
	fake.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		// The "server" comes up on the socket the spec names.
		for _, m := range c.Spec.HostConfig.Mounts {
			if m.Target == "/sock" {
				stops <- serveUnix(t, filepath.Join(m.Source, container.DistillSocketName), fakeServer(t, func(chatRequest) (int, string) { return 200, content("{}") }))
			}
		}
		return docker.Behavior{Duration: time.Hour}
	}
	// A leftover from a crash is removed first.
	stale, _ := fake.CreateContainer(context.Background(), docker.Spec{Name: "old", Labels: map[string]string{container.LabelDistill: "k"}})
	m, _ := Lookup("qwen3.5-2b")
	r := Managed{Docker: fake, Models: Store{Dir: "/models"}, SocketDir: filepath.Join(dir, "sock"), User: "1:1", CPUs: 2, Name: "icaro-distill-k", Key: "k", Poll: time.Millisecond}
	c, stop, err := r.Start(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !fake.Container(stale).Removed {
		t.Fatal("the leftover was kept")
	}
	if _, err := c.Complete(context.Background(), "s", "u", SummarySchema, 1); err != nil {
		t.Fatal(err)
	}
	var id string
	for k, ct := range fake.Containers {
		if ct.Spec.Name == "icaro-distill-k" {
			id = k
		}
	}
	spec := fake.Container(id).Spec
	if string(spec.HostConfig.NetworkMode) != "none" || spec.HostConfig.Resources.NanoCPUs != 2e9 || spec.User != "1:1" {
		t.Fatalf("spec %+v", spec.HostConfig)
	}
	stop()
	if !fake.Container(id).Removed {
		t.Fatal("stop did not remove the container")
	}
	(<-stops)()

	// A server that never comes up times out, and its container goes.
	fake.OnStart = nil
	r.ReadyTimeout = 30 * time.Millisecond
	if _, _, err := r.Start(context.Background(), m); err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatal(err)
	}
	fake.PullErr = errors.New("no registry")
	fake.Images = map[string]bool{} // not pulled yet
	if _, _, err := r.Start(context.Background(), m); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	r.SocketDir = filepath.Join(blocked, "sock")
	if _, _, err := r.Start(context.Background(), m); err == nil {
		t.Fatal("socket dir under a file")
	}
}

// FuzzParseSummary feeds arbitrary model output to the parser: no panic,
// and whatever it accepts stays within the caps.
func FuzzParseSummary(f *testing.F) {
	f.Add([]byte(`{"intent":"i","constraints":["a"],"preferences":[],"rejected":[{"what":"w","why":"y"}],"open":[],"resume":"r"}`))
	f.Add([]byte(`{"intent":"` + strings.Repeat("x", 600) + `"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, in []byte) {
		s, err := ParseSummary(in)
		if err != nil {
			return
		}
		if len([]rune(s.Intent)) > maxSentence || len([]rune(s.Resume)) > maxResume || len(s.Constraints) > maxItems ||
			len(s.Preferences) > maxItems || len(s.Open) > maxItems || len(s.Rejected) > maxItems {
			t.Fatalf("over the caps: %+v", s)
		}
	})
}
