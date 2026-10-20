package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer() *Server {
	s := NewServer("icaro", "test", "be nice")
	s.AddTool(Tool{
		Name:        "whoami",
		Description: "returns the caller project",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"string"}}}`),
		Annotations: &Annotations{ReadOnlyHint: Bool(true)},
		Handler: func(ctx context.Context, args json.RawMessage) (*ToolResult, error) {
			c, _ := CallerFrom(ctx)
			var in struct {
				ProjectID string `json:"project_id"`
			}
			if err := DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			// Arguments never override the caller.
			return JSONResult(map[string]string{"project": c.ProjectID}), nil
		},
	})
	s.AddTool(Tool{Name: "fail", Description: "x", Handler: func(context.Context, json.RawMessage) (*ToolResult, error) {
		return nil, errors.New("boom")
	}})
	s.AddTool(Tool{Name: "panic", Description: "x", Handler: func(context.Context, json.RawMessage) (*ToolResult, error) {
		panic("oops")
	}})
	s.AddTool(Tool{Name: "nil", Description: "x", Handler: func(context.Context, json.RawMessage) (*ToolResult, error) { return nil, nil }})
	s.AddResource(Resource{URI: "icaro://schema", Name: "schema", MimeType: "application/json", Read: func(context.Context) (string, error) { return `{"a":1}`, nil }})
	s.AddResource(Resource{URI: "icaro://bad", Name: "bad", Read: func(context.Context) (string, error) { return "", errors.New("nope") }})
	return s
}

func exercise(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	init, err := c.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != LatestVersion || init.ServerInfo.Name != "icaro" || init.Instructions != "be nice" || init.Capabilities["resources"] == nil {
		t.Fatalf("init = %+v", init)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 4 || tools[3].Name != "whoami" || tools[3].Annotations == nil {
		t.Fatalf("tools = %+v %v", tools, err)
	}
	res, err := c.CallTool(ctx, "whoami", map[string]any{"project_id": "evil"})
	if err != nil {
		t.Fatal(err)
	}
	var who map[string]string
	if err := Structured(res, &who); err != nil || who["project"] != "pa" {
		t.Fatalf("who = %v %v", who, err)
	}
	res, _ = c.CallTool(ctx, "whoami", map[string]any{"unknown": 1})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "unknown field") {
		t.Fatalf("strict args: %+v", res)
	}
	for _, name := range []string{"fail", "panic"} {
		res, err = c.CallTool(ctx, name, nil)
		if err != nil || !res.IsError {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
	}
	res, _ = c.CallTool(ctx, "nil", nil)
	if res.IsError || res.Content[0].Text != "ok" {
		t.Fatal("nil result")
	}
	var rpcErr *RPCError
	if _, err := c.CallTool(ctx, "nope", nil); !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("unknown tool: %v", err)
	}
	if err := c.Call(ctx, "nope/method", nil, nil); !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("unknown method: %v", err)
	}
	if err := c.Call(ctx, "ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	var rl struct {
		Resources []Resource `json:"resources"`
	}
	if err := c.Call(ctx, "resources/list", nil, &rl); err != nil || len(rl.Resources) != 2 {
		t.Fatalf("resources %v %v", rl, err)
	}
	var rr struct {
		Contents []map[string]string `json:"contents"`
	}
	if err := c.Call(ctx, "resources/read", map[string]string{"uri": "icaro://schema"}, &rr); err != nil || rr.Contents[0]["text"] != `{"a":1}` {
		t.Fatalf("read %v %v", rr, err)
	}
	if err := c.Call(ctx, "resources/read", map[string]string{"uri": "icaro://none"}, nil); err == nil {
		t.Fatal("missing resource")
	}
	if err := c.Call(ctx, "resources/read", map[string]string{"uri": "icaro://bad"}, nil); err == nil {
		t.Fatal("failing resource")
	}
}

func TestInProcess(t *testing.T) {
	exercise(t, InProcess(testServer(), Caller{ProjectID: "pa"}))
}

func TestHTTPTransport(t *testing.T) {
	s := testServer()
	h := s.HTTPHandler(func(r *http.Request) (Caller, error) {
		if r.Header.Get("X-Test") != "ok" {
			return Caller{}, errors.New("denied")
		}
		return Caller{ProjectID: "pa"}, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := HTTPClient(srv.Client(), srv.URL, http.Header{"X-Test": {"ok"}})
	exercise(t, c)

	// Session id issued on initialize.
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	if err == nil {
		_ = resp.Body.Close()
	}
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated: %v %v", resp, err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	req.Header.Set("X-Test", "ok")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get(SessionHeader) == "" || !strings.Contains(string(body), `"2025-03-26"`) {
		t.Fatalf("session/version: %v %s", resp.Header, body)
	}
	for _, tc := range []struct {
		method, body, proto string
		want                int
	}{
		{http.MethodGet, "", "", http.StatusMethodNotAllowed},
		{http.MethodPut, "", "", http.StatusMethodNotAllowed},
		{http.MethodDelete, "", "", http.StatusNoContent},
		{http.MethodPost, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", http.StatusAccepted},
		{http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, "1999-01-01", http.StatusBadRequest},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL, strings.NewReader(tc.body))
		req.Header.Set("X-Test", "ok")
		if tc.proto != "" {
			req.Header.Set(ProtocolHeader, tc.proto)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s %s: %d, want %d", tc.method, tc.body, resp.StatusCode, tc.want)
		}
	}
}

func TestStdioAndBridge(t *testing.T) {
	s := testServer()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `not json` + "\n")
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), Caller{}, in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"result":{}`) || !strings.Contains(lines[1], "-32700") {
		t.Fatalf("stdio out: %q", out.String())
	}

	srv := httptest.NewServer(s.HTTPHandler(func(*http.Request) (Caller, error) { return Caller{ProjectID: "pa"}, nil }))
	defer srv.Close()
	b := &Bridge{Client: srv.Client(), URL: srv.URL}
	in2 := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`,
	}, "\n"))
	out.Reset()
	if err := b.Run(context.Background(), in2, &out); err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `\"project\": \"pa\"`) || b.sessionID == "" {
		t.Fatalf("bridge out: %q", out.String())
	}

	// A dead endpoint yields a JSON-RPC error per request, not a hang.
	dead := &Bridge{Client: srv.Client(), URL: "http://127.0.0.1:1"}
	out.Reset()
	_ = dead.Run(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"ping"}`+"\n"+`{"jsonrpc":"2.0","method":"x"}`), &out)
	if !strings.Contains(out.String(), `"id":7`) || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("dead: %q", out.String())
	}
}

func TestMessageEdgeCases(t *testing.T) {
	s := testServer()
	ctx := context.Background()
	cases := map[string]string{
		`[]`: "-32600",
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`: `"id":1`,
		`[x`: "-32700",
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`:                                                 "-32600",
		`{"jsonrpc":"2.0","id":1}`:                                                                 "-32600",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`:                              "-32602",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":"x"}`:                              "-32602",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1900-01-01"}}`: LatestVersion,
		`{"jsonrpc":"2.0","id":1,"method":"resources/templates/list"}`:                             "resourceTemplates",
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":"x"}`:                          "-32602",
	}
	for in, want := range cases {
		out := string(s.HandleMessage(ctx, []byte(in)))
		if !strings.Contains(out, want) {
			t.Fatalf("%s => %s (want %s)", in, out, want)
		}
	}
	for _, silent := range []string{"", `{"jsonrpc":"2.0","id":1,"result":{}}`, `{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"x"}}`, `{"jsonrpc":"1.0","method":"x"}`, `[{"jsonrpc":"2.0","method":"notifications/initialized"}]`} {
		if out := s.HandleMessage(ctx, []byte(silent)); out != nil {
			t.Fatalf("%q should be silent, got %s", silent, out)
		}
	}
	if _, ok := CallerFrom(ctx); ok {
		t.Fatal("no caller")
	}
	for _, f := range []func(){
		func() {
			s.AddTool(Tool{Name: "whoami", Handler: func(context.Context, json.RawMessage) (*ToolResult, error) { return nil, nil }})
		},
		func() { s.AddTool(Tool{Name: "x"}) },
		func() {
			s.AddResource(Resource{URI: "icaro://schema", Read: func(context.Context) (string, error) { return "", nil }})
		},
		func() { s.AddResource(Resource{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			f()
		}()
	}
	if (&RPCError{Code: 1, Message: "m"}).Error() == "" || TextResult("x").Content[0].Text != "x" {
		t.Fatal("helpers")
	}
	var v map[string]any
	if err := Structured(&ToolResult{Content: []Content{{Text: `{"a":1}`}}}, &v); err != nil || v["a"] != float64(1) {
		t.Fatal("structured from text")
	}
	if err := Structured(&ToolResult{}, &v); err == nil {
		t.Fatal("empty structured")
	}
}
