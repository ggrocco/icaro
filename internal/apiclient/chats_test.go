package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icaro/internal/assist"
)

func TestChatEndpoints(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, method, uri, body string
		call                    func(c *Client) error
	}{
		{"list", "GET", "/api/v1/chats", "", func(c *Client) error { _, err := c.ListChats(ctx); return err }},
		{"create", "POST", "/api/v1/chats", `{"workflow":"hello"}`, func(c *Client) error {
			_, err := c.CreateChat(ctx, assist.CreateRequest{Workflow: "hello"})
			return err
		}},
		{"get", "GET", "/api/v1/chats/a%20b", "", func(c *Client) error { _, err := c.GetChat(ctx, "a b"); return err }},
		{"send", "POST", "/api/v1/chats/c1/messages", `{"text":"hi"}`, func(c *Client) error { _, err := c.SendChat(ctx, "c1", "hi"); return err }},
		{"cancel", "POST", "/api/v1/chats/c1/cancel", "", func(c *Client) error { return c.CancelChat(ctx, "c1") }},
		{"approve", "POST", "/api/v1/chats/c1/approvals/a1/approve", "", func(c *Client) error { _, err := c.DecideChat(ctx, "c1", "a1", true); return err }},
		{"decline", "POST", "/api/v1/chats/c1/approvals/a1/decline", "", func(c *Client) error { _, err := c.DecideChat(ctx, "c1", "a1", false); return err }},
		{"delete", "DELETE", "/api/v1/chats/c1", "", func(c *Client) error { return c.DeleteChat(ctx, "c1") }},
		{"memories", "GET", "/api/v1/chats/c1/memories", "", func(c *Client) error { _, err := c.ChatMemories(ctx, "c1"); return err }},
		{"forget", "DELETE", "/api/v1/chats/c1/memories/m1", "", func(c *Client) error { return c.ForgetChatMemory(ctx, "c1", "m1") }},
		{"distill", "POST", "/api/v1/chats/c1/distill", "", func(c *Client) error { return c.DistillChat(ctx, "c1") }},
		{"stats", "GET", "/api/v1/assistant/stats", "", func(c *Client) error { _, err := c.AssistantStats(ctx); return err }},
	} {
		c, rec := fakeServer(t, `{}`)
		if err := tc.call(c); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if rec.method != tc.method || rec.uri != tc.uri || strings.TrimSpace(rec.body) != tc.body {
			t.Errorf("%s: %+v", tc.name, rec)
		}
	}
}

func TestChatEvents(t *testing.T) {
	var gotURI, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotAuth = r.URL.RequestURI(), r.Header.Get("Authorization")
		if strings.Contains(r.URL.Path, "/missing/") {
			http.Error(w, `{"error":"chat not found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, ": keep-alive\n\n"+
			"event: entry\nid: 10\ndata: {\"type\":\"a\"}\n\n"+
			"event: other\ndata: {}\n\n"+
			"event: entry\nid: 20\ndata: {\"type\":\"b\"}\n\n"+
			"event: resync\nid: 20\ndata: {}\n\n"+
			"event: entry\nid: 30\ndata: {\"type\":\"never\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	var got []ChatEntry
	if err := c.ChatEvents(context.Background(), "c1", 5, func(e ChatEntry) bool { got = append(got, e); return true }); err != nil {
		t.Fatal(err)
	}
	if gotURI != "/api/v1/chats/c1/events?offset=5" || gotAuth != "Bearer tok" {
		t.Fatalf("%s %s", gotURI, gotAuth)
	}
	if len(got) != 3 || got[0].Offset != 10 || string(got[1].Data) != `{"type":"b"}` || !got[2].Resync {
		t.Fatalf("%+v", got)
	}
	// fn can stop early.
	n := 0
	_ = c.ChatEvents(context.Background(), "c1", 0, func(ChatEntry) bool { n++; return false })
	if n != 1 {
		t.Fatal(n)
	}
	if err := c.ChatEvents(context.Background(), "missing", 0, func(ChatEntry) bool { return true }); !IsNotFound(err) {
		t.Fatalf("404: %v", err)
	}
	// The stream ends without a resync: the context's error, if any.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.ChatEvents(ctx, "c1", 0, func(ChatEntry) bool { return true }); err == nil {
		t.Fatal("cancelled context")
	}
	if err := New("http://[::1", "").ChatEvents(context.Background(), "c1", 0, nil); err == nil {
		t.Fatal("bad base URL")
	}
	if err := New("http://127.0.0.1:1", "").ChatEvents(context.Background(), "c1", 0, nil); err == nil {
		t.Fatal("unreachable server")
	}
}
