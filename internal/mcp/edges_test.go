package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("reset") }

func TestResultHelpers(t *testing.T) {
	if r := JSONResult(map[string]any{"c": make(chan int)}); !r.IsError || !strings.Contains(r.Content[0].Text, "encode result") {
		t.Fatalf("unencodable result: %+v", r)
	}
	if b := mustJSON(make(chan int)); !strings.Contains(string(b), "-32603") {
		t.Fatalf("mustJSON fallback: %s", b)
	}
	if got := string(trimSpace([]byte("\t\r\n x \n\t"))); got != "x" {
		t.Fatalf("trim: %q", got)
	}

	var v map[string]any
	if err := Structured(JSONResult(map[string]int{"a": 1}), &v); err != nil || v["a"] != float64(1) {
		t.Fatalf("structured from a Go value: %v %v", v, err)
	}
	if err := Structured(&ToolResult{StructuredContent: make(chan int)}, &v); err == nil {
		t.Fatal("unencodable structured content")
	}
}

func TestDispatchEdges(t *testing.T) {
	s := testServer()
	s.AddResource(Resource{URI: "icaro://plain", Name: "plain", Read: func(context.Context) (string, error) { return "hi", nil }})
	ctx := context.Background()
	for in, want := range map[string]string{
		`{"jsonrpc":"2.0","id":1,"method":5}`:                                                 "-32600",
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"icaro://plain"}}`: `"mimeType":"text/plain"`,
	} {
		if out := string(s.HandleMessage(ctx, []byte(in))); !strings.Contains(out, want) {
			t.Fatalf("%s => %s (want %s)", in, out, want)
		}
	}
}

func TestServeStdioFailures(t *testing.T) {
	s := testServer()
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ServeStdio(ctx, Caller{}, strings.NewReader(ping), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if err := s.ServeStdio(context.Background(), Caller{}, strings.NewReader(ping), failWriter{}); err == nil {
		t.Fatal("write failure must end the loop")
	}
	if err := s.ServeStdio(context.Background(), Caller{}, failReader{}, io.Discard); err == nil {
		t.Fatal("read failure must be returned")
	}
}

func TestHTTPHandlerRejections(t *testing.T) {
	s := testServer()
	h := s.HTTPHandler(func(r *http.Request) (Caller, error) {
		if r.Header.Get("Authorization") == "" {
			return Caller{}, errors.New("token required")
		}
		return Caller{}, nil
	})
	post := func(body io.Reader, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", body)
		if auth {
			req.Header.Set("Authorization", "Bearer x")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(strings.NewReader(`{}`), false); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "token required") {
		t.Fatalf("unauthenticated: %d %s", rec.Code, rec.Body)
	}
	if rec := post(failReader{}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable body: %d", rec.Code)
	}
	if rec := post(bytes.NewReader(make([]byte, maxMessage+1)), true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", rec.Code)
	}
}

func TestBridgeFailures(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.HTTPHandler(func(*http.Request) (Caller, error) { return Caller{}, nil }))
	defer srv.Close()
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

	// Blank lines are skipped; a write failure ends the bridge.
	b := &Bridge{Client: srv.Client(), URL: srv.URL}
	if err := b.Run(context.Background(), strings.NewReader("\n  \n"+ping), failWriter{}); err == nil {
		t.Fatal("write failure must be returned")
	}

	// A cancelled context ends the bridge instead of answering.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := b.Run(ctx, strings.NewReader(ping), &out); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancelled: %v %q", err, out.String())
	}

	if _, err := (&Bridge{Client: srv.Client(), URL: "http://bad\x7f"}).Post(context.Background(), []byte(ping)); err == nil {
		t.Fatal("invalid URL")
	}

	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no session", http.StatusForbidden)
	}))
	defer forbidden.Close()
	hb := &Bridge{Client: forbidden.Client(), URL: forbidden.URL, Header: http.Header{"Authorization": {"Bearer t"}}}
	if _, err := hb.Post(context.Background(), []byte(ping)); err == nil || !strings.Contains(err.Error(), "HTTP 403: no session") {
		t.Fatalf("status error: %v", err)
	}
}

func TestClientFailures(t *testing.T) {
	ctx := context.Background()
	sendErr := &Client{Send: func(context.Context, []byte) ([]byte, error) { return nil, errors.New("down") }}
	if err := sendErr.Call(ctx, "ping", nil, nil); err == nil || err.Error() != "down" {
		t.Fatalf("send: %v", err)
	}
	if _, err := sendErr.Initialize(ctx); err == nil {
		t.Fatal("initialize must fail")
	}
	silent := &Client{Send: func(context.Context, []byte) ([]byte, error) { return nil, nil }}
	if err := silent.Call(ctx, "ping", nil, nil); err == nil || !strings.Contains(err.Error(), "no reply") {
		t.Fatalf("no reply: %v", err)
	}
	garbled := &Client{Send: func(context.Context, []byte) ([]byte, error) { return []byte("nope"), nil }}
	if err := garbled.Call(ctx, "ping", nil, nil); err == nil || !strings.Contains(err.Error(), "decode reply") {
		t.Fatalf("garbled: %v", err)
	}
	if _, err := garbled.CallTool(ctx, "x", nil); err == nil {
		t.Fatal("call tool must fail")
	}
}
