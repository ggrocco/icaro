package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
			}
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"type": r.Header.Get("Content-Type"), "body": string(body)})
		case "/invalid":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"invalid workflow","issues":[{"path":"/steps/0","message":"bad","hint":"fix"},{"path":"/name","message":"missing"}]}`))
		case "/fields":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"missing trigger field(s)","fields":["issue"]}`))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := New(srv.URL+"/", "tok")
	if c.BaseURL != srv.URL {
		t.Fatalf("trailing slash kept: %s", c.BaseURL)
	}
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := NewWithHTTP(srv.URL, "", srv.Client()).Health(ctx); err == nil || err.Error() != "Unauthorized (HTTP 401)" {
		t.Fatalf("no token: %v", err)
	}

	var echo map[string]string
	if err := c.Do(ctx, http.MethodPost, "/echo", map[string]int{"n": 1}, &echo); err != nil || echo["type"] != "application/json" || echo["body"] != `{"n":1}` {
		t.Fatalf("%v %v", echo, err)
	}
	var raw []byte
	if err := c.Send(ctx, http.MethodPost, "/echo", "text/yaml", strings.NewReader("a: 1"), &raw); err != nil || !strings.Contains(string(raw), `"type":"text/yaml"`) {
		t.Fatalf("%s %v", raw, err)
	}
	if err := c.Do(ctx, http.MethodGet, "/empty", nil, &echo); err != nil {
		t.Fatalf("an empty body decodes nothing: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, "/echo", func() {}, nil); err == nil {
		t.Fatal("a body that cannot be encoded")
	}

	err := c.Do(ctx, http.MethodPost, "/invalid", nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusUnprocessableEntity || len(e.Issues) != 2 {
		t.Fatalf("%#v", err)
	}
	if want := "invalid workflow:\n  /steps/0: bad (hint: fix)\n  /name: missing"; err.Error() != want {
		t.Fatalf("%q", err.Error())
	}
	if err := c.Do(ctx, http.MethodPost, "/fields", nil, nil); !errors.As(err, &e) || e.Fields[0] != "issue" {
		t.Fatalf("%#v", err)
	}
	if err := c.Do(ctx, http.MethodGet, "/nope", nil, nil); !IsNotFound(err) || IsNotFound(errors.New("x")) {
		t.Fatalf("%v", err)
	}
	if err := c.Send(ctx, "BAD METHOD", "/", "", nil, nil); err == nil {
		t.Fatal("an invalid request")
	}
	if err := New("http://127.0.0.1:1", "").Health(ctx); err == nil {
		t.Fatal("an unreachable server")
	}
}
