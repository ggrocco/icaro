package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/apiclient"
	"icaro/internal/crypto"
	"icaro/internal/mcp"
	"icaro/internal/memory"
	"icaro/internal/service"
	"icaro/internal/store"
)

func TestMemoryREST(t *testing.T) {
	ctx := context.Background()
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
	srvAPI := New(service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet}), quiet)
	eng := memory.New(st, quiet, "test")
	host := mcp.NewServer("icaro", "test", "")
	memory.RegisterTools(host, eng)
	RegisterMemory(srvAPI, eng, host)
	srv := httptest.NewServer(srvAPI.Handler())
	t.Cleanup(srv.Close)
	mk := func(name, scope string) (*apiclient.Client, string) {
		plain, hash, _ := crypto.NewToken()
		if _, err := st.CreateToken(ctx, name, hash, scope); err != nil {
			t.Fatal(err)
		}
		return apiclient.New(srv.URL, plain), plain
	}
	admin, adminTok := mk("admin", store.ScopeAdmin)
	reader, readerTok := mk("reader", store.ScopeRead)

	m, err := admin.WriteMemory(ctx, memory.RESTWrite{Project: "pa", Kind: "decision", Title: "Ship the REST fallback", Body: "for non-MCP agents", Agent: "aider", Origin: "cli", TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if m.Origin != memory.OriginCLI || m.Source != "agent:aider" || m.CreatedAt.IsZero() || m.ExpiresAt == nil {
		t.Fatalf("write: %+v", m)
	}
	if _, err := reader.WriteMemory(ctx, memory.RESTWrite{Project: "pa", Kind: "task", Title: "x"}); err == nil {
		t.Fatal("read token wrote")
	}
	if _, err := admin.WriteMemory(ctx, memory.RESTWrite{Kind: "task", Title: "x"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := admin.WriteMemory(ctx, memory.RESTWrite{Project: "pa", Kind: "file", Title: "x"}); err == nil {
		t.Fatal("ingest kind via REST")
	}

	res, err := reader.QueryMemory(ctx, apiclient.MemoryQuery{Project: "pa", Text: "rest fallback", Kinds: []string{"decision"}, Limit: 5})
	if err != nil || res.Count != 1 || res.Results[0].Source == "" || res.Timestamp.IsZero() {
		t.Fatalf("query: %+v %v", res, err)
	}
	if res, _ := reader.QueryMemory(ctx, apiclient.MemoryQuery{Project: "pb", Text: "rest fallback"}); res.Count != 0 {
		t.Fatal("project B sees A over REST")
	}
	if _, err := reader.MemoryProvenance(ctx, "pb", m.ID); !apiclient.IsNotFound(err) {
		t.Fatalf("cross-project provenance: %v", err)
	}
	p, err := reader.MemoryProvenance(ctx, "pa", m.ID)
	if err != nil || p.ID != m.ID {
		t.Fatalf("provenance %+v %v", p, err)
	}

	h, _ := admin.WriteMemory(ctx, memory.RESTWrite{Project: "pa", Kind: "handoff", Title: "Wire the attacher", Branch: "main"})
	pend, err := reader.PendingHandoffs(ctx, "pa", "main")
	if err != nil || pend.Count != 1 {
		t.Fatalf("pending %+v %v", pend, err)
	}
	acc, err := admin.AcceptHandoff(ctx, memory.RESTAccept{Project: "pa", Agent: "codex"}, h.ID)
	if err != nil || acc.Status != memory.StatusAccepted {
		t.Fatalf("accept %+v %v", acc, err)
	}
	if _, err := admin.AcceptHandoff(ctx, memory.RESTAccept{Project: "pa"}, h.ID); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("double accept: %v", err)
	}
	if _, err := admin.AcceptHandoff(ctx, memory.RESTAccept{Project: "pb"}, h.ID); !apiclient.IsNotFound(err) {
		t.Fatalf("cross accept: %v", err)
	}

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Hello() {}\n"), 0o600)
	ing, err := admin.IngestMemory(ctx, memory.RESTIngest{Path: dir, Commits: -1})
	if err != nil || ing.Report.Files != 1 || !strings.HasPrefix(ing.Project, "p_") {
		t.Fatalf("ingest %+v %v", ing, err)
	}
	if res, _ := reader.QueryMemory(ctx, apiclient.MemoryQuery{Project: ing.Project, Text: "hello"}); res.Count == 0 {
		t.Fatal("ingested symbol not found")
	}
	if _, err := admin.IngestMemory(ctx, memory.RESTIngest{}); err == nil {
		t.Fatal("ingest without path")
	}
	imp, err := admin.ImportMemory(ctx, memory.RESTImport{Project: "pa", Format: "ai-memory", Data: `[{"id":"z","title":"Imported note"}]`})
	if err != nil || imp.Imported != 1 {
		t.Fatalf("import %+v %v", imp, err)
	}
	if _, err := admin.ImportMemory(ctx, memory.RESTImport{Project: "pa", Format: "nope", Data: "{}"}); err == nil {
		t.Fatal("bad format")
	}
	for _, path := range []string{"/api/v1/memory?project=pa&since=x", "/api/v1/memory?project=bad%20id", "/api/v1/memory/handoffs", "/api/v1/memory/x"} {
		var out []byte
		if err := reader.Do(ctx, "GET", path, nil, &out); err == nil {
			t.Fatalf("%s accepted", path)
		}
	}

	// Host MCP endpoint: scope from the header, auth from the token.
	hdr := http.Header{"Authorization": {"Bearer " + adminTok}, HeaderProject: {"pa"}, HeaderAgent: {"claude"}}
	c := mcp.HTTPClient(srv.Client(), srv.URL+"/mcp", hdr)
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := c.CallTool(ctx, memory.ToolQuery, map[string]any{"text": "rest fallback"})
	var qr memory.QueryResponse
	if err != nil || mcp.Structured(r, &qr) != nil || qr.Count != 1 {
		t.Fatalf("mcp query: %+v %v", qr, err)
	}
	ro := mcp.HTTPClient(srv.Client(), srv.URL+"/mcp", http.Header{"Authorization": {"Bearer " + readerTok}, HeaderProject: {"pa"}})
	if r, err := ro.CallTool(ctx, memory.ToolWrite, map[string]any{"kind": "task", "title": "x"}); err != nil || !r.IsError {
		t.Fatalf("read token wrote over MCP: %+v %v", r, err)
	}
	anon := mcp.HTTPClient(srv.Client(), srv.URL+"/mcp", nil)
	if _, err := anon.Initialize(ctx); err == nil {
		t.Fatal("unauthenticated /mcp")
	}
	bad := mcp.HTTPClient(srv.Client(), srv.URL+"/mcp", http.Header{"Authorization": {"Bearer " + adminTok}, HeaderProject: {"bad id"}})
	if _, err := bad.Initialize(ctx); err == nil {
		t.Fatal("bad project header")
	}
}
