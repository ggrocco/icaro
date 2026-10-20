package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"icaro/internal/store"
)

// engines runs fn on SQLite and, with ICARO_TEST_POSTGRES_DSN, on a
// private Postgres schema.
func engines(t *testing.T, fn func(t *testing.T, e *Engine)) {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("sqlite", func(t *testing.T) {
		db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		fn(t, New(db, quiet, "test"))
	})
	dsn := os.Getenv("ICARO_TEST_POSTGRES_DSN")
	if dsn == "" {
		return
	}
	t.Run("postgres", func(t *testing.T) {
		schema := "test_memory_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
		admin, err := store.Open(ctx, store.Postgres, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.DB().ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema); err != nil {
			t.Fatal(err)
		}
		_ = admin.Close()
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		db, err := store.Open(ctx, store.Postgres, dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		fn(t, New(db, quiet, "test"))
	})
}

var (
	scopeA  = Scope{ProjectID: "pa", Branch: "main", Agent: "claude", SessionID: "s1", Origin: OriginAgent}
	scopeA2 = Scope{ProjectID: "pa", Branch: "feature", Agent: "codex", SessionID: "s2", Origin: OriginCLI}
	scopeB  = Scope{ProjectID: "pb", Branch: "main", Agent: "claude", SessionID: "s3", Origin: OriginAgent}
)

func TestTokenize(t *testing.T) {
	got := Tokenize("ParseTemplate parses the HTTPServer's snake_case decisions, 2 ids at v2!")
	want := []string{"parsetemplate", "parse", "template", "parse", "httpserver", "http", "server", "snakecase", "snake", "case", "decision", "ids", "v2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %v\nwant       %v", got, want)
	}
	for in, out := range map[string]string{"policies": "policy", "matches": "match", "boxes": "box", "class": "class", "status": "status", "analysis": "analysis", "runs": "run"} {
		if stem(in) != out {
			t.Fatalf("stem(%s) = %s", in, stem(in))
		}
	}
	if len(uniqueTerms("a b c dd dd ee", 2)) != 2 {
		t.Fatal("uniqueTerms cap")
	}
}

func TestWriteQueryScope(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		d, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindDecision, Title: "Use SQLite with WAL", Body: "Single writer goroutine; Postgres stays portable.", Metadata: map[string]any{"adr": "0001"}})
		if err != nil {
			t.Fatal(err)
		}
		if d.Source != "session:s1" || d.CreatedAt.IsZero() || d.Link != "icaro://memory/"+d.ID || d.Scope.ProjectID != "pa" || d.Origin != OriginAgent {
			t.Fatalf("stamped fields: %+v", d)
		}
		if _, err := e.Write(ctx, scopeA2, WriteRequest{Kind: KindFinding, Body: "SQLite busy_timeout avoids lock errors\nmore detail", Source: "internal/store/store.go", SourceLine: 52}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindTask, Title: "Add memory benchmark"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write(ctx, scopeB, WriteRequest{Kind: KindDecision, Title: "Use SQLite everywhere", Body: "project b secret plan"}); err != nil {
			t.Fatal(err)
		}

		res, err := e.Query(ctx, scopeA, Query{Text: "sqlite"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 || res[0].Title != "Use SQLite with WAL" || res[0].Score <= 0 {
			t.Fatalf("ranked results: %+v", res)
		}
		for _, m := range res {
			if m.Scope.ProjectID != "pa" || m.Source == "" || m.CreatedAt.IsZero() {
				t.Fatalf("result without scope/source/timestamp: %+v", m)
			}
			if strings.Contains(m.Body, "project b") {
				t.Fatal("project B leaked into project A")
			}
		}
		if res[1].Title != "SQLite busy_timeout avoids lock errors" {
			t.Fatalf("title from first body line: %q", res[1].Title)
		}
		// Plural/stemmed and path-segment matches.
		if res, _ := e.Query(ctx, scopeA, Query{Text: "lock"}); len(res) != 1 {
			t.Fatalf("body match: %d", len(res))
		}
		if res, _ := e.Query(ctx, scopeA, Query{Text: "store"}); len(res) != 1 {
			t.Fatal("source path terms are searchable")
		}
		if res, _ := e.Query(ctx, scopeA, Query{Text: "!!!"}); len(res) != 0 {
			t.Fatal("no-term query")
		}
		if res, _ := e.Query(ctx, scopeA, Query{Text: "nothingmatches"}); len(res) != 0 {
			t.Fatal("no hits")
		}
		// Filters.
		if res, _ := e.Query(ctx, scopeA, Query{Kinds: []string{KindTask}}); len(res) != 1 || res[0].Kind != KindTask {
			t.Fatal("kind filter")
		}
		if res, _ := e.Query(ctx, scopeA, Query{Agent: "codex"}); len(res) != 1 {
			t.Fatal("agent filter")
		}
		if res, _ := e.Query(ctx, scopeA, Query{Branch: "feature"}); len(res) != 1 {
			t.Fatal("branch filter")
		}
		if res, _ := e.Query(ctx, scopeA, Query{SourcePrefix: "internal/"}); len(res) != 1 {
			t.Fatal("source prefix")
		}
		future := time.Now().Add(time.Hour)
		if res, _ := e.Query(ctx, scopeA, Query{Since: &future}); len(res) != 0 {
			t.Fatal("since")
		}
		if res, _ := e.Query(ctx, scopeA, Query{}); len(res) != 3 {
			t.Fatalf("list all in A: %d", len(res))
		}
		if _, err := e.Query(ctx, scopeA, Query{Kinds: []string{"bogus"}}); !errors.Is(err, ErrInvalid) {
			t.Fatal("bogus kind")
		}

		// Provenance never crosses projects.
		if _, err := e.Provenance(ctx, scopeB, d.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-project provenance: %v", err)
		}
		p, err := e.Provenance(ctx, scopeA, d.ID)
		if err != nil || p.Metadata["adr"] != "0001" || p.Edges == nil {
			t.Fatalf("provenance: %+v %v", p, err)
		}

		// Supersedes must point inside the project.
		if _, err := e.Write(ctx, scopeB, WriteRequest{Kind: KindDecision, Title: "x", Supersedes: d.ID}); !errors.Is(err, ErrInvalid) {
			t.Fatal("supersedes across projects")
		}
		s2, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindDecision, Title: "Use SQLite with WAL2", Supersedes: d.ID})
		if err != nil {
			t.Fatal(err)
		}
		if p, _ := e.Provenance(ctx, scopeA, s2.ID); len(p.Edges) != 1 || p.Edges[0].Kind != "supersedes" {
			t.Fatalf("edges: %+v", p.Edges)
		}
	})
}

func TestWriteValidation(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		bad := []struct {
			scope Scope
			req   WriteRequest
		}{
			{Scope{}, WriteRequest{Kind: KindTask, Title: "x"}},
			{scopeA, WriteRequest{Kind: KindFile, Title: "x"}},
			{scopeA, WriteRequest{Kind: KindTask}},
			{scopeA, WriteRequest{Kind: KindTask, Title: strings.Repeat("x", 301)}},
			{scopeA, WriteRequest{Kind: KindTask, Title: "x", Body: strings.Repeat("x", maxBody+1)}},
			{scopeA, WriteRequest{Kind: KindTask, Title: "x", TTL: -1}},
			{scopeA, WriteRequest{Kind: KindTask, Title: "x", Metadata: map[string]any{"x": strings.Repeat("y", 17<<10)}}},
			{scopeA, WriteRequest{Kind: KindTask, Title: "x", Metadata: map[string]any{"f": func() {}}}},
		}
		for i, b := range bad {
			if _, err := e.Write(ctx, b.scope, b.req); err == nil {
				t.Fatalf("case %d accepted", i)
			}
		}
		if _, err := e.Query(ctx, Scope{}, Query{}); err == nil {
			t.Fatal("unscoped query")
		}
		if _, err := e.Provenance(ctx, Scope{}, "x"); err == nil {
			t.Fatal("unscoped provenance")
		}
		long, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindFinding, Title: "long", Body: strings.Repeat("é", snippetRunes+10)})
		if err != nil {
			t.Fatal(err)
		}
		res, _ := e.Query(ctx, scopeA, Query{Text: "long"})
		if len(res) != 1 || !res[0].Truncated {
			t.Fatal("query bodies are snippets")
		}
		p, _ := e.Provenance(ctx, scopeA, long.ID)
		if p.Truncated {
			t.Fatal("provenance is full")
		}
	})
}

func TestHandoffAcrossSessions(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		h, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindHandoff, Title: "Pick up the DAG executor", Body: "retry done; cancel left"})
		if err != nil || h.Status != StatusPending {
			t.Fatalf("%+v %v", h, err)
		}
		// Another CLI (different session, agent, branch) recovers it.
		pending, err := e.PendingHandoffs(ctx, scopeA2, "")
		if err != nil || len(pending) != 1 || pending[0].ID != h.ID {
			t.Fatalf("pending: %+v %v", pending, err)
		}
		if p, _ := e.PendingHandoffs(ctx, scopeA2, "feature"); len(p) != 0 {
			t.Fatal("branch-scoped handoff filter")
		}
		if p, _ := e.PendingHandoffs(ctx, scopeB, ""); len(p) != 0 {
			t.Fatal("handoff leaked to project B")
		}
		if _, err := e.AcceptHandoff(ctx, scopeB, h.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("project B accepted A's handoff")
		}
		acc, err := e.AcceptHandoff(ctx, scopeA2, h.ID)
		if err != nil || acc.Status != StatusAccepted || acc.AcceptedBy != "codex@session:s2" || acc.AcceptedAt == nil {
			t.Fatalf("accept: %+v %v", acc, err)
		}
		if _, err := e.AcceptHandoff(ctx, scopeA, h.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("double accept: %v", err)
		}
		task, _ := e.Write(ctx, scopeA, WriteRequest{Kind: KindTask, Title: "t"})
		if _, err := e.AcceptHandoff(ctx, scopeA, task.ID); !errors.Is(err, ErrConflict) {
			t.Fatal("accepting a task")
		}
		if _, err := e.AcceptHandoff(ctx, Scope{ProjectID: "pa", Origin: OriginCLI}, task.ID); !errors.Is(err, ErrConflict) {
			t.Fatal("accept by origin")
		}
		if p, _ := e.PendingHandoffs(ctx, scopeA, ""); len(p) != 0 {
			t.Fatal("accepted handoff still pending")
		}
		if _, err := e.AcceptHandoff(ctx, Scope{}, h.ID); err == nil {
			t.Fatal("unscoped accept")
		}
	})
}

func TestTTLExpiry(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		now := time.Now().UTC()
		e.SetClock(func() time.Time { return now })
		m, err := e.Write(ctx, scopeA, WriteRequest{Kind: KindFinding, Title: "temporary note", TTL: time.Minute})
		if err != nil || m.ExpiresAt == nil {
			t.Fatal(err)
		}
		if res, _ := e.Query(ctx, scopeA, Query{Text: "temporary"}); len(res) != 1 {
			t.Fatal("visible before expiry")
		}
		e.SetClock(func() time.Time { return now.Add(2 * time.Minute) })
		if res, _ := e.Query(ctx, scopeA, Query{Text: "temporary"}); len(res) != 0 {
			t.Fatal("expired memory returned")
		}
		if _, err := e.Provenance(ctx, scopeA, m.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("expired provenance")
		}
		n, err := e.Expire(ctx)
		if err != nil || n != 1 {
			t.Fatalf("expire %d %v", n, err)
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := e.Run(cctx); err != nil || e.Name() == "" {
			t.Fatal("run")
		}
	})
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=Tester", "add", "-A"},
		{"-c", "user.email=t@t", "-c", "user.name=Tester", "commit", "-q", "-m", "Initial import of the parser\n\nBody line"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // test fixture commands
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIngest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		root := t.TempDir()
		writeFiles(t, root, map[string]string{
			"go.mod": "module demo\n",
			"parser/parse.go": `// Package parser parses things.
package parser

import (
	"fmt"
	"strings"
)

// MaxDepth bounds recursion.
const MaxDepth = 8

// Tree is a parse tree.
type Tree struct{}

// ParseTemplate parses a template string.
func ParseTemplate(s string) (*Tree, error) { return nil, fmt.Errorf("%s", strings.TrimSpace(s)) }

func (t *Tree) Walk() {}
`,
			"web/app.ts":                  "import { h } from 'preact'\nconst x = require('lodash')\nexport function renderApp() {}\nexport class Widget {}\nexport interface Props {}\n",
			"tools/gen.py":                "import os\nfrom pathlib import Path\n\nclass Generator:\n    def run(self):\n        pass\n",
			"docs/adr/0001-use-sqlite.md": "# ADR 0001 — Use SQLite\n\n**Status:** Accepted\n\nWe use SQLite.\n",
			"node_modules/x/index.js":     "export function ignored() {}",
			".hidden/secret.txt":          "nope",
			"bin.dat":                     "a\x00b",
		})
		gitInit(t, root)
		pid, err := ProjectIDForPath(root)
		if err != nil {
			t.Fatal(err)
		}
		scope := Scope{ProjectID: pid, Origin: OriginIngest}
		rep, err := e.Ingest(ctx, scope, root, IngestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Files != 5 || rep.ADRs != 1 || rep.Commits != 1 || rep.Symbols < 9 || rep.Skipped != 1 {
			t.Fatalf("report: %+v", rep)
		}
		hit := func(q Query) []*Memory {
			t.Helper()
			res, err := e.Query(ctx, scope, q)
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		sym := hit(Query{Text: "ParseTemplate", Kinds: []string{KindSymbol}})
		if len(sym) == 0 || sym[0].Title != "parser.ParseTemplate" || sym[0].SourceLine != 16 || !strings.HasPrefix(sym[0].Link, "file://") {
			t.Fatalf("go symbol: %+v", sym)
		}
		if m := hit(Query{Text: "walk", Kinds: []string{KindSymbol}}); len(m) != 1 || m[0].Title != "parser.Tree.Walk" {
			t.Fatalf("method: %+v", m)
		}
		if m := hit(Query{Text: "widget renderApp", Kinds: []string{KindSymbol}}); len(m) < 2 || !isTS(m[0].Title) || !isTS(m[1].Title) {
			t.Fatalf("ts symbols: %v", titles(m))
		}
		if m := hit(Query{Text: "generator", Kinds: []string{KindSymbol}}); len(m) != 1 {
			t.Fatal("py symbols")
		}
		adr := hit(Query{Kinds: []string{KindADR}})
		if len(adr) != 1 || adr[0].Title != "ADR 0001 — Use SQLite" || adr[0].Metadata["status"] != "Accepted" {
			t.Fatalf("adr: %+v", adr)
		}
		commits := hit(Query{Text: "parser import", Kinds: []string{KindCommit}})
		if len(commits) != 1 || !strings.HasPrefix(commits[0].Source, "commit:") {
			t.Fatalf("commit: %+v", commits)
		}
		files := hit(Query{Kinds: []string{KindFile}, SourcePrefix: "parser/"})
		if len(files) != 1 {
			t.Fatal("file memory")
		}
		p, _ := e.Provenance(ctx, scope, files[0].ID)
		var imports, defines int
		for _, ed := range p.Edges {
			switch ed.Kind {
			case "imports":
				imports++
			case "defines":
				defines++
			}
		}
		if imports != 2 || defines != 4 {
			t.Fatalf("edges: %+v", p.Edges)
		}
		if m := hit(Query{Text: "ignored"}); len(m) != 0 {
			t.Fatal("node_modules indexed")
		}

		// Idempotent re-ingest.
		rep2, err := e.Ingest(ctx, scope, root, IngestOptions{})
		if err != nil || rep2.Files != 0 || rep2.Unchanged != 5 || rep2.Removed != 0 || rep2.Commits != 0 {
			t.Fatalf("re-ingest: %+v %v", rep2, err)
		}
		// Changes and deletions are reflected.
		_ = os.Remove(filepath.Join(root, "tools/gen.py"))
		writeFiles(t, root, map[string]string{"web/app.ts": "export function renamed() {}\n"})
		rep3, err := e.Ingest(ctx, scope, root, IngestOptions{Commits: -1})
		if err != nil || rep3.Files != 1 || rep3.Removed < 4 {
			t.Fatalf("change: %+v %v", rep3, err)
		}
		if m := hit(Query{Text: "generator"}); len(m) != 0 {
			t.Fatal("deleted file still in memory")
		}
		if m := hit(Query{Text: "widget"}); len(m) != 0 {
			t.Fatal("removed symbol still in memory")
		}
		// Subtree ingest does not remove facts outside the subtree.
		if _, err := e.Ingest(ctx, scope, filepath.Join(root, "docs"), IngestOptions{Commits: -1}); err != nil {
			t.Fatal(err)
		}
		if m := hit(Query{Text: "ParseTemplate"}); len(m) == 0 {
			t.Fatal("subtree ingest removed other files")
		}
		if _, err := e.Ingest(ctx, scope, filepath.Join(root, "missing"), IngestOptions{}); !errors.Is(err, ErrInvalid) {
			t.Fatal("missing root")
		}
		if _, err := e.Ingest(ctx, Scope{}, root, IngestOptions{}); err == nil {
			t.Fatal("unscoped ingest")
		}
		// Project B sees none of it.
		if res, _ := e.Query(ctx, scopeB, Query{Text: "ParseTemplate"}); len(res) != 0 {
			t.Fatal("ingest leaked")
		}
	})
}

func TestProjectID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "x", "sub/b.txt": "y"})
	gitInit(t, root)
	a, _ := ProjectIDForPath(root)
	b, _ := ProjectIDForPath(filepath.Join(root, "sub"))
	if a != b || !strings.HasPrefix(a, "p_") || len(a) != 18 {
		t.Fatalf("ids %s %s", a, b)
	}
	// A linked worktree shares the project.
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "feat", wt).CombinedOutput(); err != nil { //nolint:gosec // test fixture command
		t.Fatalf("%v %s", err, out)
	}
	c, _ := ProjectIDForPath(wt)
	if c != a {
		t.Fatalf("worktree project %s != %s", c, a)
	}
	if GitBranch(wt) != "feat" || GitTopLevel(filepath.Join(wt)) == "" {
		t.Fatal("branch/toplevel")
	}
	plain := t.TempDir()
	d, _ := ProjectIDForPath(plain)
	if d == a || GitBranch(plain) != "" {
		t.Fatal("non-repo dir")
	}
	// ProjectIDForPath resolves symlinks (macOS temp dirs live under the
	// /var -> /private/var link); the root hash takes the resolved path.
	resolved, err := filepath.EvalSymlinks(plain)
	if err != nil {
		t.Fatal(err)
	}
	if ProjectIDForRoot(resolved) != d {
		t.Fatal("root hash")
	}
}

func TestImporters(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		read := func(name string) []byte {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		rep, err := e.Import(ctx, scopeA, "graphify", read("graphify.json"))
		if err != nil || rep.Imported != 3 || rep.Edges != 3 {
			t.Fatalf("graphify: %+v %v", rep, err)
		}
		res, _ := e.Query(ctx, scopeA, Query{Text: "credentials"})
		if len(res) != 1 || res[0].Kind != KindSymbol || res[0].Source != "src/auth.py" || res[0].SourceLine != 12 || res[0].Origin != "import:graphify" {
			t.Fatalf("graphify node: %+v", res)
		}
		if res, _ := e.Query(ctx, scopeA, Query{Kinds: []string{KindDecision}}); len(res) != 1 {
			t.Fatal("graphify decision")
		}
		p, _ := e.Provenance(ctx, scopeA, res[0].ID)
		if len(p.Edges) != 2 {
			t.Fatalf("graphify edges: %+v", p.Edges)
		}
		again, _ := e.Import(ctx, scopeA, "graphify", read("graphify.json"))
		if again.Imported != 0 || again.Unchanged != 3 {
			t.Fatalf("re-import: %+v", again)
		}

		rep, err = e.Import(ctx, scopeA, "ai-memory", read("ai-memory.json"))
		if err != nil || rep.Imported != 2 {
			t.Fatalf("ai-memory: %+v %v", rep, err)
		}
		res, _ = e.Query(ctx, scopeA, Query{Text: "pgx"})
		if len(res) != 1 || res[0].Kind != KindDecision || res[0].CreatedAt.Year() != 2026 || res[0].Metadata["namespace"] != "icaro" {
			t.Fatalf("ai-memory decision: %+v", res)
		}
		rep, err = e.Import(ctx, scopeA, "ai-memory", read("ai-memory.jsonl"))
		if err != nil || rep.Imported != 2 {
			t.Fatalf("jsonl: %+v %v", rep, err)
		}
		if res, _ := e.PendingHandoffs(ctx, scopeA, ""); len(res) != 1 || !strings.HasPrefix(res[0].Title, "Handoff") {
			t.Fatalf("imported handoff: %+v", res)
		}
		if _, err := e.Import(ctx, scopeA, "ai-memory", []byte(`[{"id":"x","title":"arr"}]`)); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []struct{ f, d string }{{"nope", "{}"}, {"graphify", "x"}, {"graphify", "{}"}, {"graphify", `{"nodes":[{}]}`}, {"ai-memory", ""}, {"ai-memory", "{x"}} {
			if _, err := e.Import(ctx, scopeA, bad.f, []byte(bad.d)); err == nil {
				t.Fatalf("%v accepted", bad)
			}
		}
		if _, err := e.Import(ctx, Scope{}, "graphify", nil); err == nil {
			t.Fatal("unscoped import")
		}
		if res, _ := e.Query(ctx, scopeB, Query{}); len(res) != 0 {
			t.Fatal("import leaked")
		}
		if len(ImportFormats()) != 2 || lineOf("x.go:7") != 7 || lineOf(float64(3)) != 3 || lineOf(nil) != 0 {
			t.Fatal("helpers")
		}
	})
}

func titles(ms []*Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Title
	}
	return out
}

func isTS(title string) bool { return title == "renderApp" || title == "Widget" }

// Regression: ingesting a subtree through a symlinked path (every macOS
// temp dir, any repo reached through a link) must not treat the subtree as
// the whole project and drop the facts of every other file.
func TestIngestSubtreeThroughSymlink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		real := t.TempDir()
		writeFiles(t, real, map[string]string{
			"go.mod":       "module demo\n",
			"lib/lib.go":   "package lib\n\n// Frobnicate does things.\nfunc Frobnicate() {}\n",
			"docs/note.md": "# Note\n\nAbout gizmos.\n",
		})
		gitInit(t, real)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(real, link); err != nil {
			t.Skip("symlinks unsupported: ", err)
		}
		pid, err := ProjectIDForPath(link)
		if err != nil {
			t.Fatal(err)
		}
		scope := Scope{ProjectID: pid, Origin: OriginIngest}
		if _, err := e.Ingest(ctx, scope, link, IngestOptions{Commits: -1}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Ingest(ctx, scope, filepath.Join(link, "docs"), IngestOptions{Commits: -1}); err != nil {
			t.Fatal(err)
		}
		res, err := e.Query(ctx, scope, Query{Text: "Frobnicate"})
		if err != nil || len(res) == 0 {
			t.Fatalf("subtree ingest through a symlink dropped lib/: %v %d", err, len(res))
		}
	})
}
