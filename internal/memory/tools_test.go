package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/mcp"
	"icaro/internal/session"
)

func unixHTTP(sock string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func call(t *testing.T, c *mcp.Client, tool string, args any, out any) *mcp.ToolResult {
	t.Helper()
	res, err := c.CallTool(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if out != nil && !res.IsError {
		if err := mcp.Structured(res, out); err != nil {
			t.Fatalf("%s decode: %v", tool, err)
		}
	}
	return res
}

func TestMCPToolsEnforceScope(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		srv := e.NewMCPServer("preamble")
		a := mcp.InProcess(srv, mcp.Caller{ProjectID: "pa", Branch: "main", Agent: "claude", SessionID: "s1", Scope: "write"})
		b := mcp.InProcess(srv, mcp.Caller{ProjectID: "pb", Agent: "claude", SessionID: "s9", Scope: "write"})
		init, err := a.Initialize(ctx)
		if err != nil || init.Instructions != "preamble" {
			t.Fatalf("init %+v %v", init, err)
		}
		tools, _ := a.ListTools(ctx)
		if len(tools) != 3 {
			t.Fatalf("exactly three tools, got %d", len(tools))
		}

		var m Memory
		call(t, a, ToolWrite, map[string]any{"kind": "decision", "title": "Adopt MCP for memory", "body": "agents speak MCP", "ttl_seconds": 3600, "metadata": map[string]any{"k": "v"}}, &m)
		if m.Scope.ProjectID != "pa" || m.Scope.SessionID != "s1" || m.Source == "" || m.CreatedAt.IsZero() || m.ExpiresAt == nil {
			t.Fatalf("written: %+v", m)
		}
		// Asking for another project's scope is refused, not honored.
		res := call(t, b, ToolWrite, map[string]any{"kind": "finding", "title": "x", "project_id": "pa"}, nil)
		if !res.IsError || !strings.Contains(res.Content[0].Text, "project_id") {
			t.Fatalf("project_id argument must be refused: %+v", res)
		}
		res = call(t, b, ToolQuery, map[string]any{"text": "mcp", "project_id": "pa"}, nil)
		if !res.IsError {
			t.Fatal("query project_id argument must be refused")
		}
		var qb QueryResponse
		call(t, b, ToolQuery, map[string]any{"text": "adopt mcp memory"}, &qb)
		if qb.Count != 0 || qb.Scope.ProjectID != "pb" {
			t.Fatalf("project B sees A: %+v", qb)
		}
		res = call(t, b, ToolProvenance, map[string]any{"id": m.ID}, nil)
		if !res.IsError {
			t.Fatal("project B read A's provenance")
		}

		var qa QueryResponse
		call(t, a, ToolQuery, map[string]any{"text": "adopt mcp", "kinds": []string{"decision"}, "since": "2000-01-01T00:00:00Z", "limit": 5}, &qa)
		if qa.Count != 1 || qa.Results[0].Source == "" || qa.Results[0].CreatedAt.IsZero() || qa.Timestamp.IsZero() || qa.Results[0].Scope.ProjectID != "pa" {
			t.Fatalf("query: %+v", qa)
		}
		if res := call(t, a, ToolQuery, map[string]any{"since": "yesterday"}, nil); !res.IsError {
			t.Fatal("bad since")
		}
		var p Provenance
		call(t, a, ToolProvenance, map[string]any{"id": m.ID}, &p)
		if p.ID != m.ID || p.Origin != OriginAgent || p.Link == "" {
			t.Fatalf("provenance %+v", p)
		}

		// Handoff via MCP: written in one session, accepted from another.
		var h Memory
		call(t, a, ToolWrite, map[string]any{"kind": "handoff", "title": "Continue the ingest work"}, &h)
		c := mcp.InProcess(srv, mcp.Caller{ProjectID: "pa", Agent: "codex", SessionID: "s2", Scope: "write"})
		var pend QueryResponse
		call(t, c, ToolQuery, map[string]any{"kinds": []string{"handoff"}, "status": "pending"}, &pend)
		if pend.Count != 1 || pend.Results[0].ID != h.ID {
			t.Fatalf("pending: %+v", pend)
		}
		call(t, c, ToolProvenance, map[string]any{"id": h.ID, "accept": true}, &p)
		if p.Status != StatusAccepted || p.AcceptedBy != "codex@session:s2" {
			t.Fatalf("accepted: %+v", p)
		}

		// Read-only tokens can query but not write or accept.
		ro := mcp.InProcess(srv, mcp.Caller{ProjectID: "pa", Scope: "read"})
		if res := call(t, ro, ToolWrite, map[string]any{"kind": "task", "title": "x"}, nil); !res.IsError {
			t.Fatal("read token wrote")
		}
		if res := call(t, ro, ToolProvenance, map[string]any{"id": h.ID, "accept": true}, nil); !res.IsError {
			t.Fatal("read token accepted")
		}
		call(t, ro, ToolQuery, map[string]any{}, &qa)
		// No scope at all.
		none := mcp.InProcess(srv, mcp.Caller{})
		if res := call(t, none, ToolQuery, map[string]any{}, nil); !res.IsError || !strings.Contains(res.Content[0].Text, "no project scope") {
			t.Fatalf("unscoped: %+v", res)
		}
		for _, tool := range []string{ToolWrite, ToolProvenance} {
			if res := call(t, none, tool, map[string]any{}, nil); !res.IsError {
				t.Fatalf("%s unscoped", tool)
			}
		}
		if res := call(t, a, ToolWrite, map[string]any{"kind": "file", "title": "x"}, nil); !res.IsError {
			t.Fatal("agents cannot write ingest kinds")
		}
		if res := call(t, a, ToolQuery, map[string]any{"kinds": []string{"nope"}}, nil); !res.IsError {
			t.Fatal("bad kind")
		}
		if res := call(t, a, ToolProvenance, map[string]any{"id": "missing", "accept": true}, nil); !res.IsError {
			t.Fatal("accept missing")
		}
	})
}

func TestServeSessionSocketAndBridge(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e.SetSessionInstructions("accept handoffs first")
		dir, err := os.MkdirTemp("/tmp", "icaro-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		sockA, sockB := filepath.Join(dir, "a.sock"), filepath.Join(dir, "b.sock")
		stopA, err := e.ServeSession(ctx, session.Identity{SessionID: "sa", ProjectID: "pa", Branch: "main", Agent: "claude"}, sockA)
		if err != nil {
			t.Fatal(err)
		}
		defer stopA()
		stopB, err := e.ServeSession(ctx, session.Identity{SessionID: "sb", ProjectID: "pb", Agent: "claude"}, sockB)
		if err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(sockA); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode %v %v", fi, err)
		}

		// The stdio bridge an agent CLI launches inside the container.
		in := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"x"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_write","arguments":{"kind":"finding","title":"Socket bridge works","body":"via stdio"}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_query","arguments":{"text":"socket bridge"}}}`,
		}, "\n")
		var out bytes.Buffer
		br := &mcp.Bridge{Client: unixHTTP(sockA), URL: "http://icaro/mcp"}
		if err := br.Run(ctx, strings.NewReader(in), &out); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 3 || !strings.Contains(lines[0], "accept handoffs first") {
			t.Fatalf("bridge replies: %q", out.String())
		}
		var reply struct {
			Result struct {
				StructuredContent QueryResponse `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(lines[2]), &reply); err != nil {
			t.Fatal(err)
		}
		q := reply.Result.StructuredContent
		if q.Count != 1 || q.Scope.ProjectID != "pa" || q.Results[0].Scope.SessionID != "sa" || q.Results[0].Source != "session:sa" {
			t.Fatalf("socket query: %+v", q)
		}

		// Session B's socket cannot see A, whatever it asks for.
		cb := mcp.HTTPClient(unixHTTP(sockB), "http://icaro/", nil)
		if _, err := cb.Initialize(ctx); err != nil {
			t.Fatal(err)
		}
		var qb QueryResponse
		call(t, cb, ToolQuery, map[string]any{"text": "socket bridge"}, &qb)
		if qb.Count != 0 || qb.Scope.ProjectID != "pb" {
			t.Fatalf("leak through socket: %+v", qb)
		}

		stopB()
		stopB() // idempotent
		if _, err := os.Stat(sockB); !os.IsNotExist(err) {
			t.Fatal("socket not removed on stop")
		}
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(sockA); os.IsNotExist(err) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := os.Stat(sockA); !os.IsNotExist(err) {
			t.Fatal("socket not removed on ctx cancel")
		}
		if _, err := e.ServeSession(context.Background(), session.Identity{SessionID: "x"}, filepath.Join(dir, "x.sock")); err == nil {
			t.Fatal("session without project")
		}
		if _, err := e.ServeSession(context.Background(), session.Identity{SessionID: "x", ProjectID: "p"}, "/tmp/"+strings.Repeat("x", 120)); err == nil || !strings.Contains(err.Error(), "bytes") {
			t.Fatalf("long socket path: %v", err)
		}
	})
}
