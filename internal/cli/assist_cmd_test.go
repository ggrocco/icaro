package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/assist"
)

const chatDetail = `{"id":"c1","title":"bug to slack","profile":"claude","agent":"claude","workflow":"hello","session_status":"running",
 "created_at":"2026-11-10T10:00:00Z","updated_at":"2026-11-10T10:00:00Z",
 "messages":[{"id":"u1","seq":1,"role":"user","text":"post to slack","status":"done","log_offset":0,"created_at":"2026-11-10T10:00:00Z"},
             {"id":"m1","seq":2,"role":"assistant","text":"partial","status":"rate_limited","error":"limit","log_offset":0,"created_at":"2026-11-10T10:00:00Z"}],
 "draft":{"yaml":"name: hello","issues":[{"path":"/steps"}],"source":"assistant","version":3,"updated_at":"2026-11-10T10:00:00Z"},
 "approvals":[{"id":"a1","tool":"run_workflow","request":{"workflow":"hello"},"created_at":"2026-11-10T10:00:00Z","expires_at":"2026-11-10T10:05:00Z"}],
 "log_size":10}`

func TestAssistCommands(t *testing.T) {
	stream := "event: entry\nid: 5\ndata: {\"type\":\"status\",\"message_id\":\"m2\",\"status\":\"running\"}\n\n" +
		"event: entry\nid: 6\ndata: {\"type\":\"status\",\"message_id\":\"m2\",\"status\":\"queued\",\"note\":\"Waiting for a free assistant slot.\"}\n\n" +
		"event: entry\nid: 7\ndata: {\"type\":\"event\",\"message_id\":\"other\",\"event\":{\"kind\":\"text\",\"text\":\"not mine\"}}\n\n" +
		"event: entry\nid: 8\ndata: not json\n\n" +
		"event: entry\nid: 9\ndata: {\"type\":\"event\",\"message_id\":\"m2\",\"event\":{\"kind\":\"text\",\"text\":\"Checking \"}}\n\n" +
		"event: entry\nid: 10\ndata: {\"type\":\"event\",\"message_id\":\"m2\",\"event\":{\"kind\":\"tool_call\",\"tool\":\"validate_workflow\"}}\n\n" +
		"event: entry\nid: 11\ndata: {\"type\":\"approval\",\"approval\":{\"id\":\"a9\",\"tool\":\"run_workflow\",\"request\":{\"workflow\":\"hello\"}}}\n\n" +
		"event: entry\nid: 12\ndata: {\"type\":\"event\",\"message_id\":\"m2\",\"event\":{\"kind\":\"text\",\"text\":\"done.\"}}\n\n" +
		"event: entry\nid: 13\ndata: {\"type\":\"status\",\"message_id\":\"m2\",\"status\":\"done\",\"message\":{\"id\":\"m2\",\"status\":\"done\",\"text\":\"Checking done.\"}}\n\n"
	fakeAPI(t, map[string]string{
		"GET /api/v1/chats":                          `{"chats":[{"id":"c1","title":"","agent":"claude","updated_at":"2026-11-10T10:00:00Z"}]}`,
		"POST /api/v1/chats":                         chatDetail,
		"GET /api/v1/chats/c1":                       chatDetail,
		"POST /api/v1/chats/c1/messages":             `{"messages":[{"id":"u2","role":"user","text":"hi","status":"done"},{"id":"m2","role":"assistant","status":"queued","log_offset":4}]}`,
		"GET /api/v1/chats/c1/events":                stream,
		"POST /api/v1/chats/c1/cancel":               ``,
		"POST /api/v1/chats/c1/approvals/a1/approve": `{"id":"a1","decision":"approved"}`,
		"POST /api/v1/chats/c1/approvals/a1/decline": `{"id":"a1","decision":"declined"}`,
		"DELETE /api/v1/chats/c1":                    ``,
		"POST /api/v1/chats/c2/messages":             `{"messages":[{"id":"u3","role":"user"},{"id":"m3","role":"assistant","status":"queued","log_offset":0}]}`,
		"GET /api/v1/chats/c2/events":                "event: entry\nid: 3\ndata: {\"type\":\"status\",\"message_id\":\"m3\",\"status\":\"auth_required\",\"note\":\"Please run /login\"}\n\n",
	})
	wantCLI(t, []string{"assist", "list"}, "ID", "c1", "claude", "-")
	wantCLI(t, []string{"assist", "list", "--json"}, `"id": "c1"`)
	draft := filepath.Join(t.TempDir(), "d.yaml")
	_ = os.WriteFile(draft, []byte("name: x\n"), 0o600)
	wantCLI(t, []string{"assist", "new", "--workflow", "hello", "-f", draft}, "c1")
	wantCLI(t, []string{"assist", "new", "--json"}, `"workflow": "hello"`)
	failCLI(t, "no such file", "assist", "new", "-f", filepath.Join(t.TempDir(), "missing"))
	wantCLI(t, []string{"assist", "show", "c1"}, "bug to slack", "[you] post to slack", "(rate_limited) limit", "draft v3 by assistant, 1 issue(s)", "icaro assist approve c1 a1")
	wantCLI(t, []string{"assist", "show", "c1", "--json"}, `"log_size": 10`)
	out := wantCLI(t, []string{"assist", "send", "c1", "hi", "there"}, "Waiting for a free assistant slot.", "Checking ", "→ validate_workflow", "[waiting for you] run_workflow", "icaro assist approve c1 a9", "done.")
	if strings.Contains(out, "not mine") {
		t.Errorf("another message's text was printed:\n%s", out)
	}
	wantCLI(t, []string{"assist", "send", "c1", "hi", "--json"}, `"text": "Checking done."`)
	wantCLI(t, []string{"assist", "send", "c1", "hi", "--no-wait"}, "queued m2")
	wantCLI(t, []string{"assist", "send", "c1", "hi", "--no-wait", "--json"}, `"id": "m2"`)
	failCLI(t, "not signed in", "assist", "send", "c2", "hi")
	mustCLI(t, "assist", "cancel", "c1")
	wantCLI(t, []string{"assist", "approve", "c1", "a1"}, "approved")
	wantCLI(t, []string{"assist", "decline", "c1", "a1", "--json"}, `"declined"`)
	wantCLI(t, []string{"assist", "rm", "c1"}, "Delete chat c1, its container and its log? [y/N]") // no answer: nothing is deleted
	mustCLI(t, "assist", "rm", "c1", "--yes")
	failCLI(t, "not found", "assist", "show", "nope")
	failCLI(t, "not found", "assist", "send", "nope", "x")
	failCLI(t, "not found", "assist", "cancel", "nope")
	failCLI(t, "not found", "assist", "approve", "nope", "a")
	failCLI(t, "not found", "assist", "rm", "nope", "--yes")
}

func TestReplyError(t *testing.T) {
	for _, tc := range []struct {
		status, want string
	}{{"error", "the reply error: boom"}, {"rate_limited", "usage limit"}, {"cancelled", "cancelled"}} {
		if err := replyError(&assist.MessageInfo{Status: tc.status, Error: "boom"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.status, err)
		}
	}
	reset := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	if err := replyError(&assist.MessageInfo{Status: "rate_limited", ResetAt: &reset}); err == nil || !strings.Contains(err.Error(), "resets at") {
		t.Error(err)
	}
	if replyError(&assist.MessageInfo{Status: "done"}) != nil {
		t.Error("done is not an error")
	}
}
