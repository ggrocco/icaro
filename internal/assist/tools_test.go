package assist

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"icaro/internal/mcp"
	"icaro/internal/session"
	"icaro/internal/store"
)

// chatWithSession makes a chat bound to an assist session, as a first
// turn would.
func (e *env) chatWithSession(yaml string) (*ChatDetail, string) {
	e.t.Helper()
	d, err := e.s.CreateChat(e.ctx, CreateRequest{YAML: yaml}, "local")
	if err != nil {
		e.t.Fatal(err)
	}
	sess, _ := e.sess.CreateAssist(e.ctx, session.AssistRequest{ChatID: d.ID})
	if err := e.st.SetChatField(e.ctx, d.ID, store.ChatSessionID, sess.ID); err != nil {
		e.t.Fatal(err)
	}
	return d, sess.ID
}

func TestDraftTools(t *testing.T) {
	e := newEnv(t)
	empty, sid := e.chatWithSession("")
	out, isErr := e.tool(sid, ToolGetDraft, map[string]any{})
	if isErr || out["version"].(float64) != 0 || !strings.Contains(out["note"].(string), "empty") {
		t.Fatalf("empty draft %+v", out)
	}

	// An invalid proposal is still shown, with its issues.
	out, isErr = e.tool(sid, ToolPropose, map[string]any{"yaml": "name: broken\nsteps: []\n", "summary": "first try"})
	if isErr || out["valid"] != false || out["version"].(float64) != 1 || len(out["issues"].([]any)) == 0 {
		t.Fatalf("invalid proposal %+v", out)
	}
	out, isErr = e.tool(sid, ToolPropose, map[string]any{"yaml": helloYAML})
	if isErr || out["valid"] != true || out["version"].(float64) != 2 {
		t.Fatalf("valid proposal %+v", out)
	}
	out, _ = e.tool(sid, ToolGetDraft, map[string]any{})
	if out["yaml"] != helloYAML || out["source"] != "assistant" || out["version"].(float64) != 2 || out["workflow"] != "broken" {
		t.Fatalf("draft after proposals %+v", out)
	}
	// Nothing was saved: proposing never writes a workflow.
	if _, err := e.svc.GetWorkflow(e.ctx, "hello"); err == nil {
		t.Fatal("propose_workflow saved a workflow")
	}
	// The person's edit is what get_draft returns next.
	if d, err := e.s.PutDraft(e.ctx, empty.ID, helloYAML+"# edited\n"); err != nil || d.Source != "user" || d.Version != 3 {
		t.Fatal(d, err)
	}
	out, _ = e.tool(sid, ToolGetDraft, map[string]any{})
	if !strings.HasSuffix(out["yaml"].(string), "# edited\n") || out["source"] != "user" {
		t.Fatalf("%+v", out)
	}
	drafts := 0
	for _, en := range e.entries(empty.ID) {
		if en.Type == EntryDraft {
			drafts++
		}
	}
	if drafts != 3 {
		t.Fatalf("%d draft entries", drafts)
	}

	for _, args := range []map[string]any{{"yaml": "  "}, {"yaml": strings.Repeat("x", maxDraft+1)}, {"yaml": 3}} {
		if out, isErr := e.tool(sid, ToolPropose, args); !isErr {
			t.Errorf("%v accepted: %+v", args, out)
		}
	}
}

func TestToolsNeedAnAssistantChat(t *testing.T) {
	e := newEnv(t)
	for _, caller := range []mcp.Caller{{SessionID: "x", Principal: "session:x"}, {Principal: "assist:"}, {SessionID: "nochat", Principal: "assist:nochat"}} {
		c := mcp.InProcess(e.mcp, caller)
		for _, name := range []string{ToolGetDraft, ToolPropose, ToolRun} {
			res, err := c.CallTool(e.ctx, name, map[string]any{"yaml": helloYAML, "name": "hello"})
			if err != nil || !res.IsError {
				t.Errorf("%+v %s: %+v %v", caller, name, res, err)
			}
		}
	}
}

func TestGatedRun(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.ApplyWorkflow(e.ctx, helloYAML, "local"); err != nil {
		t.Fatal(err)
	}
	d, sid := e.chatWithSession("")

	if out, isErr := e.tool(sid, ToolRun, map[string]any{"name": "nope"}); !isErr || !strings.Contains(out["error"].(string), "not saved") {
		t.Fatalf("unknown workflow %+v", out)
	}

	// Approved: the run starts, attributed to the person and the chat.
	type res struct {
		out   map[string]any
		isErr bool
	}
	call := func(args map[string]any) <-chan res {
		ch := make(chan res, 1)
		go func() {
			out, isErr := e.tool(sid, ToolRun, args)
			ch <- res{out, isErr}
		}()
		return ch
	}
	pending := func() ApprovalInfo {
		t.Helper()
		var ap ApprovalInfo
		e.waitFor("an approval card", func() bool {
			full, _ := e.s.GetChat(e.ctx, d.ID)
			if len(full.Approvals) == 1 {
				ap = full.Approvals[0]
				return true
			}
			return false
		})
		return ap
	}
	ch := call(map[string]any{"name": "hello", "trigger": map[string]any{"who": "chat"}})
	ap := pending()
	if !strings.Contains(string(ap.Request), `"who":"chat"`) || ap.Tool != ToolRun {
		t.Fatalf("%+v", ap)
	}
	if _, err := e.s.Decide(e.ctx, "other-chat", ap.ID, true, "local"); err != ErrNotFound {
		t.Fatalf("another chat's approval: %v", err)
	}
	got, err := e.s.Decide(e.ctx, d.ID, ap.ID, true, "alice")
	if err != nil || got.Decision != DecisionApproved {
		t.Fatal(got, err)
	}
	r := <-ch
	if r.isErr || r.out["status"] != "started" || r.out["run_id"] == "" {
		t.Fatalf("%+v", r)
	}
	run, err := e.svc.GetRun(e.ctx, r.out["run_id"].(string))
	if err != nil || !strings.Contains(string(run.Trigger), "chat") {
		t.Fatal(run, err)
	}
	if a, _ := e.st.GetChatApproval(e.ctx, ap.ID); a.Result != run.ID || a.Actor != "alice" {
		t.Fatalf("%+v", a)
	}
	if _, err := e.s.Decide(e.ctx, d.ID, ap.ID, false, "bob"); err == nil || !strings.Contains(err.Error(), "already decided") {
		t.Fatalf("second decision: %v", err)
	}
	if _, err := e.s.Decide(e.ctx, d.ID, "nope", true, "x"); err != ErrNotFound {
		t.Fatal(err)
	}

	// Declined: nothing runs.
	before, _ := e.svc.ListRuns(e.ctx, "hello", "", 100)
	ch = call(map[string]any{"name": "hello"})
	ap = pending()
	if _, err := e.s.Decide(e.ctx, d.ID, ap.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	if r := <-ch; r.isErr || r.out["status"] != "declined" {
		t.Fatalf("%+v", r)
	}
	after, _ := e.svc.ListRuns(e.ctx, "hello", "", 100)
	if len(after) != len(before) {
		t.Fatal("a declined call started a run")
	}

	// Nobody answers: the call expires.
	e.cfg.Assistant.ApprovalTimeout = 30 * time.Millisecond
	if r := <-call(map[string]any{"name": "hello"}); r.isErr || r.out["status"] != "expired" {
		t.Fatalf("%+v", r)
	}
	if full, _ := e.s.GetChat(e.ctx, d.ID); len(full.Approvals) != 0 {
		t.Fatalf("still pending: %+v", full.Approvals)
	}
}

func TestGatedRunCancelled(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.ApplyWorkflow(e.ctx, helloYAML, "local"); err != nil {
		t.Fatal(err)
	}
	d, sid := e.chatWithSession("")
	ctx, cancel := context.WithCancel(e.ctx)
	c := mcp.InProcess(e.mcp, mcp.Caller{SessionID: sid, Principal: "assist:" + sid})
	done := make(chan bool, 1)
	go func() {
		res, err := c.CallTool(ctx, ToolRun, map[string]any{"name": "hello"})
		done <- err != nil || res.IsError
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if failed := <-done; !failed {
		t.Fatal("a cancelled call succeeded")
	}
	// The card it left is expired, so nobody can approve it later.
	e.waitFor("the card to expire", func() bool {
		aps, _ := e.st.ListChatApprovals(e.ctx, d.ID, false)
		return len(aps) == 1 && aps[0].Decision == DecisionExpired
	})
}

func TestAttacher(t *testing.T) {
	e := newEnv(t)
	_, sid := e.chatWithSession(helloYAML)
	dir := t.TempDir()
	if len(dir) > 80 {
		dir = "/tmp"
	}
	stop, err := e.s.Attacher(e.mcp).ServeAssist(e.ctx, session.Identity{SessionID: sid, Agent: "claude"}, dir+"/a-"+sid+".sock")
	if err != nil {
		t.Fatal(err)
	}
	stop()
	srv := e.s.Server("v", func(s *mcp.Server) {
		s.AddTool(mcp.Tool{Name: "extra", Description: "x", Handler: func(context.Context, json.RawMessage) (*mcp.ToolResult, error) { return nil, nil }})
	})
	names := srv.ToolNames()
	if len(names) != 4 {
		t.Fatalf("%v", names)
	}
}
