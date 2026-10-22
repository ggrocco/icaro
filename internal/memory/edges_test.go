package memory

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/mcp"
	"icaro/internal/session"
	"icaro/internal/store"
)

func sqliteEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "test"), db
}

func TestEngineSurfacesStoreFailures(t *testing.T) {
	ctx := context.Background()
	e, db := sqliteEngine(t)
	m, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindHandoff, Title: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if _, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindFinding, Title: "x"}); err == nil {
		t.Fatal("write")
	}
	if _, err := e.Query(ctx, scopeA, Query{}); err == nil {
		t.Fatal("list query")
	}
	if _, err := e.Query(ctx, scopeA, Query{Text: "pending"}); err == nil {
		t.Fatal("text query")
	}
	if _, err := e.Provenance(ctx, scopeA, m.ID); err == nil {
		t.Fatal("provenance")
	}
	if _, err := e.AcceptHandoff(ctx, scopeA, m.ID); err == nil {
		t.Fatal("accept")
	}
	if _, err := e.Expire(ctx); err == nil {
		t.Fatal("expire")
	}
	if _, err := e.Import(ctx, scopeA, "ai-memory", []byte(`[{"id":"a","title":"t"}]`)); err == nil {
		t.Fatal("import")
	}
	if _, err := e.Ingest(ctx, scopeA, t.TempDir(), IngestOptions{}); err == nil {
		t.Fatal("ingest")
	}

	// The expiry loop logs and keeps going; it ends with its context.
	var logs bytes.Buffer
	e.log = slog.New(slog.NewTextHandler(&logs, nil))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Run(runCtx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "memory expiry") {
		t.Fatalf("expiry failure must be logged: %s", logs.String())
	}
}

func TestRunExpiresImmediately(t *testing.T) {
	ctx := context.Background()
	e, _ := sqliteEngine(t)
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return now })
	if _, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindFinding, Title: "short lived", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	lines := make(chan string, 16)
	e.log = slog.New(slog.NewTextHandler(chanWriter(lines), nil))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Run(runCtx) }()
	select {
	case l := <-lines:
		if !strings.Contains(l, "expired memories") || !strings.Contains(l, "count=1") {
			t.Fatalf("the first pass runs at startup: %s", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no expiry log")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// chanWriter hands each log line to the test goroutine.
type chanWriter chan string

func (c chanWriter) Write(p []byte) (int, error) {
	c <- string(p)
	return len(p), nil
}

func TestScopeAndDefaults(t *testing.T) {
	ctx := context.Background()
	sc := ScopeFromIdentity(session.Identity{SessionID: "s", ProjectID: "p", Branch: "b", Agent: "claude"}, OriginAgent)
	if sc != (Scope{ProjectID: "p", Branch: "b", Agent: "claude", SessionID: "s", Origin: OriginAgent}) {
		t.Fatalf("%+v", sc)
	}
	if New(nil, nil, "v").log == nil {
		t.Fatal("a nil logger falls back to the default")
	}

	e, _ := sqliteEngine(t)
	agentOnly := Scope{ProjectID: "p", Agent: "codex"}
	m, err := e.Write(ctx, agentOnly, WriteRequest{Kind: KindDecision, Body: strings.Repeat("é", 200) + "\nsecond line"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "agent:codex" || len([]rune(m.Title)) != 120 {
		t.Fatalf("agent source / first-line title capped at 120 runes: %q %d", m.Source, len([]rune(m.Title)))
	}
	m, err = e.Write(ctx, Scope{ProjectID: "p", Origin: OriginCLI}, WriteRequest{Kind: KindTask, Title: "t"})
	if err != nil || m.Source != OriginCLI {
		t.Fatalf("origin source: %+v %v", m, err)
	}
	if _, err := e.Write(ctx, Scope{ProjectID: "p"}, WriteRequest{Kind: KindTask, Title: "t", Metadata: map[string]any{"c": make(chan int)}}); err == nil {
		t.Fatal("unencodable metadata")
	}
	// An oversized limit is clamped instead of rejected.
	if got, err := e.Query(ctx, Scope{ProjectID: "p"}, Query{Limit: 5000}); err != nil || len(got) != 2 {
		t.Fatalf("clamped limit: %v %v", len(got), err)
	}
	if got := splitIdent("snake__case"); strings.Join(got, ",") != "snake,case" {
		t.Fatalf("split: %v", got)
	}
}

// When a common term's postings are capped and the filter removes every
// capped candidate, the query falls back to exact ranking so filtered
// matches are still found.
func TestQueryCappedPostingsFallback(t *testing.T) {
	ctx := context.Background()
	e, db := sqliteEngine(t)
	now := time.Now().UTC()
	var batch []store.MemoryBundle
	put := func(row *store.MemoryRow) {
		batch = append(batch, bundle(row))
		if len(batch) == 500 {
			if err := db.PutMemories(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	for i := 0; i < postingCap+10; i++ {
		put(&store.MemoryRow{ID: fmt.Sprintf("x%05d", i), ProjectID: "p", Branch: "busy", Origin: OriginAPI, Kind: KindFinding,
			Title: "zebra zebra zebra", Source: "api", Link: "l", CreatedAt: now, UpdatedAt: now, Metadata: "{}"})
	}
	put(&store.MemoryRow{ID: "needle", ProjectID: "p", Branch: "quiet", Origin: OriginAPI, Kind: KindFinding,
		Title: "a zebra once", Body: strings.Repeat("filler words here ", 50), Source: "api", Link: "l", CreatedAt: now, UpdatedAt: now, Metadata: "{}"})
	if err := db.PutMemories(ctx, batch); err != nil {
		t.Fatal(err)
	}
	got, err := e.Query(ctx, Scope{ProjectID: "p"}, Query{Text: "zebra", Branch: "quiet", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "needle" {
		t.Fatalf("filtered match lost behind the posting cap: %v", titles(got))
	}
}

func TestImportEdges(t *testing.T) {
	ctx := context.Background()
	e, _ := sqliteEngine(t)
	graph := `{"nodes":[
		{"id":"t1","type":"todo","label":"do it"},
		{"id":"a1","type":"adr","label":"` + strings.Repeat("long ", 100) + `"},
		{"id":"n1","type":"concept","line":12}
	],"links":[{"source":"t1","target":"a1"},{"source":"t1"}]}`
	rep, err := e.Import(ctx, scopeA, "graphify", []byte(graph))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 3 || rep.Edges != 1 {
		t.Fatalf("a link without a target is dropped: %+v", rep)
	}
	ms, _ := e.Query(ctx, scopeA, Query{Limit: 10})
	kinds := map[string]*Memory{}
	for _, m := range ms {
		kinds[m.Kind] = m
	}
	if kinds[KindTask] == nil || kinds[KindADR] == nil || len([]rune(kinds[KindADR].Title)) != maxTitle {
		t.Fatalf("graph kinds / truncated title: %+v", titles(ms))
	}
	if d := kinds[KindDecision]; d == nil || d.Title != "n1" {
		t.Fatalf("a node without label or body is titled by its key: %+v", d)
	}

	// ai-memory: numeric ids, missing ids, and an unknown kind fall back.
	rep, err = e.Import(ctx, scopeA2, "ai-memory", []byte(`{"memories":[{"id":7,"content":"numeric id"},{"content":"no id","kind":"rumor"}]}`))
	if err != nil || rep.Imported != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	ms, _ = e.Query(ctx, scopeA2, Query{Text: "numeric", Limit: 10})
	if len(ms) != 1 || !strings.HasSuffix(ms[0].Source, ":7") || ms[0].Kind != KindFinding {
		t.Fatalf("numeric id: %+v", ms)
	}
	for name, data := range map[string]string{
		"bad array":    `[{"id":`,
		"bad memories": `{"memories":[{]}`,
	} {
		if _, err := e.Import(ctx, scopeA, "ai-memory", []byte(data)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}

	// The importer interface allows any kind string; unknown ones become
	// findings rather than polluting the kind set.
	Importers["test-weird"] = weirdImporter{}
	defer delete(Importers, "test-weird")
	if _, err := e.Import(ctx, scopeB, "test-weird", nil); err != nil {
		t.Fatal(err)
	}
	ms, _ = e.Query(ctx, scopeB, Query{Limit: 10})
	if len(ms) != 1 || ms[0].Kind != KindFinding {
		t.Fatalf("unknown kind: %+v", ms)
	}
}

type weirdImporter struct{}

func (weirdImporter) Name() string { return "weird" }
func (weirdImporter) Parse([]byte) ([]ImportItem, []ImportEdge, error) {
	return []ImportItem{{Key: "k", Kind: "gossip", Title: "odd"}}, nil, nil
}

func TestIngestLimitsAndBatches(t *testing.T) {
	ctx := context.Background()
	e, _ := sqliteEngine(t)
	root := t.TempDir()
	files := map[string]string{
		"big.txt":    strings.Repeat("x", 5000),
		"broken.go":  "package x\nfunc (",
		"gen.go":     "package g\n\nvar hidden = 1\n\nfunc (l *List[T]) Push() {}\nfunc (m Map[K, V]) Get() {}\nfunc (p (Pair)) Swap() {}\n",
		"utf8.md":    strings.Repeat("a", 2047) + "é" + strings.Repeat("b", 10),
		"keep/a.txt": "kept",
	}
	for i := 0; i < 205; i++ {
		files[fmt.Sprintf("many/f%03d.txt", i)] = fmt.Sprintf("file %d", i)
	}
	writeFiles(t, root, files)
	if err := os.Symlink(filepath.Join(root, "keep", "a.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	rep, err := e.Ingest(ctx, scopeA, root, IngestOptions{MaxFileSize: 4096, Commits: -1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 209 || rep.Skipped != 1 || rep.Commits != 0 {
		t.Fatalf("one oversize file skipped, the symlink ignored, more than a batch written: %+v", rep)
	}
	got, _ := e.Query(ctx, scopeA, Query{Text: "Push Get Swap hidden", Kinds: []string{KindSymbol}, Limit: 10})
	names := strings.Join(titles(got), " ")
	for _, want := range []string{"g.List.Push", "g.Map.Get", "g.?.Swap"} {
		if !strings.Contains(names, want) {
			t.Fatalf("missing %s in %s", want, names)
		}
	}
	if strings.Contains(names, "hidden") {
		t.Fatalf("unexported vars are not symbols: %s", names)
	}
	p, err := e.Query(ctx, scopeA, Query{Kinds: []string{KindFile}, SourcePrefix: "utf8.md"})
	if err != nil || len(p) != 1 {
		t.Fatalf("utf8.md: %v %v", titles(p), err)
	}
	if pr, err := e.Provenance(ctx, scopeA, p[0].ID); err != nil || pr.Body != strings.Repeat("a", 2047) {
		t.Fatalf("a snippet never splits a UTF-8 sequence: %d %v", len(pr.Body), err)
	}

	small := t.TempDir()
	writeFiles(t, small, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"})
	rep, err = e.Ingest(ctx, scopeB, small, IngestOptions{MaxFiles: 1})
	if err != nil || rep.Files != 1 {
		t.Fatalf("max files: %+v %v", rep, err)
	}
}

func TestIngestManyCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	e, _ := sqliteEngine(t)
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", root).CombinedOutput(); err != nil { //nolint:gosec // test fixture commands
		t.Fatalf("%v: %s", err, out)
	}
	// fast-import writes 210 commits in one process.
	var stream strings.Builder
	for i := 1; i <= 210; i++ {
		fmt.Fprintf(&stream, "commit refs/heads/main\nmark :%d\ncommitter T <t@example.com> %d +0000\ndata %d\nc%03d\n", i, 1700000000+i, 4, i)
		if i > 1 {
			fmt.Fprintf(&stream, "from :%d\n", i-1)
		}
		fmt.Fprintf(&stream, "M 644 inline f%d.txt\ndata 1\nx\n\n", i)
	}
	cmd := exec.Command("git", "-C", root, "fast-import", "--quiet") //nolint:gosec // test fixture commands
	cmd.Stdin = strings.NewReader(stream.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "checkout", "-q", "-f", "main").CombinedOutput(); err != nil { //nolint:gosec // test fixture commands
		t.Fatalf("%v: %s", err, out)
	}
	rep, err := e.Ingest(ctx, scopeA, root, IngestOptions{Commits: 250})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Commits != 210 {
		t.Fatalf("commits: %+v", rep)
	}
	// Re-ingesting skips commits already stored.
	if rep, err = e.Ingest(ctx, scopeA, root, IngestOptions{Commits: 250}); err != nil || rep.Commits != 0 {
		t.Fatalf("re-ingest: %+v %v", rep, err)
	}
}

func TestProjectEdges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	plain := t.TempDir()
	if GitTopLevel(plain) != plain {
		t.Fatal("outside a repository the path is its own top level")
	}
	bare := filepath.Join(t.TempDir(), "r.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil { //nolint:gosec // test fixture commands
		t.Fatalf("%v: %s", err, out)
	}
	root, err := ProjectRoot(bare)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(bare); root != want {
		t.Fatalf("a bare repository is its own project: %s", root)
	}

	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"a": "1"})
	gitInit(t, repo)
	if out, err := exec.Command("git", "-C", repo, "checkout", "-q", "--detach").CombinedOutput(); err != nil { //nolint:gosec // test fixture commands
		t.Fatalf("%v: %s", err, out)
	}
	if b := GitBranch(repo); b != "" {
		t.Fatalf("a detached HEAD has no branch: %q", b)
	}
}

func TestServeSessionSocketErrors(t *testing.T) {
	ctx := context.Background()
	e, _ := sqliteEngine(t)
	if e.SessionServer() == nil {
		t.Fatal("session server")
	}
	id := session.Identity{SessionID: "s", ProjectID: "p"}
	dir, err := os.MkdirTemp("", "ms")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ServeSession(ctx, id, filepath.Join(file, "s.sock")); err == nil {
		t.Fatal("the socket directory cannot be created under a file")
	}
	busy := filepath.Join(dir, "busy")
	if err := os.MkdirAll(filepath.Join(busy, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ServeSession(ctx, id, busy); err == nil {
		t.Fatal("a non-empty directory at the socket path cannot be replaced")
	}
	// A stale regular file at the socket path is replaced.
	stale := filepath.Join(dir, "stale.sock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stop, err := e.ServeSession(ctx, id, stale)
	if err != nil {
		t.Fatal(err)
	}
	stop()

	// Provenance rejects malformed arguments.
	c := mcp.InProcess(e.NewMCPServer(""), mcp.Caller{ProjectID: "p", Scope: "write"})
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(ctx, ToolProvenance, map[string]any{"ident": "x"})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].Text, "unknown field") {
		t.Fatalf("%+v %v", res, err)
	}
}
