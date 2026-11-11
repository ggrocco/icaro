package assist

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/session"
	"icaro/internal/store"
)

func TestPreamble(t *testing.T) {
	p := Preamble("---\nname: icaro-workflows\ndescription: x\n---\n\n# Authoring\n\nCall create_workflow.")
	if !strings.HasPrefix(p, assistantRules) || !strings.HasSuffix(p, "# Authoring\n\nCall create_workflow.") || strings.Contains(p, "name: icaro-workflows") {
		t.Fatalf("%q", p)
	}
	if Preamble("  ") != assistantRules || !strings.HasSuffix(Preamble("plain skill"), "\n\nplain skill") {
		t.Fatal("no front matter")
	}
	if !strings.HasSuffix(Preamble("---\nunterminated"), "---\nunterminated") {
		t.Fatal("unterminated front matter")
	}
}

// A conversation of two turns: the first creates the session and names
// the conversation, the second resumes it.
func TestConversation(t *testing.T) {
	e := newEnv(t)
	d, err := e.s.CreateChat(e.ctx, CreateRequest{YAML: helloYAML}, "local")
	if err != nil {
		t.Fatal(err)
	}
	if d.Draft == nil || d.Draft.Version != 1 || d.Draft.Source != "user" || string(d.Draft.Issues) != "[]" || d.Workflow != "hello" || d.Agent != "claude" {
		t.Fatalf("created %+v draft %+v", d.ChatInfo, d.Draft)
	}
	e.sess.push(fixture(t, "claude", "tools"), fixture(t, "claude", "text"))
	msgs, err := e.s.Send(e.ctx, d.ID, "Post to Slack when an issue is labelled bug\nwith details", "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != store.RoleUser || msgs[1].Status != store.TurnQueued {
		t.Fatalf("%+v", msgs)
	}
	got := e.waitMessage(d.ID, msgs[1].ID)
	if got.Status != store.TurnDone || got.Text != "Let me check the schema and integrations.The draft needs a step; I proposed one." || got.Usage == nil || got.Usage.Input != 5000 || got.CostUSD != nil {
		t.Fatalf("%+v", got)
	}
	c, _ := e.st.GetChat(e.ctx, d.ID)
	if c.ConversationID != "3f0c6b2e-8a51-4c1e-9d7a-0d9f1a2b3c4d" || c.SessionID != "sess1" || c.Title != "Post to Slack when an issue is labelled bug" {
		t.Fatalf("chat %+v", c)
	}
	turns := e.sess.turns()
	if j := slices.Index(turns[0].Cmd, "--session-id"); j < 0 || len(turns[0].Cmd[j+1]) != 36 || slices.Contains(turns[0].Cmd, "--resume") {
		t.Fatalf("first turn %v", turns[0].Cmd)
	}
	if string(turns[0].Stdin) != "Post to Slack when an issue is labelled bug\nwith details" || !slices.Contains(turns[0].Env, "MCP_TOOL_TIMEOUT=1800000") {
		t.Fatalf("stdin %q env %v", turns[0].Stdin, turns[0].Env)
	}

	msgs, err = e.s.Send(e.ctx, d.ID, "thanks", "local")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.waitMessage(d.ID, msgs[1].ID); got.Status != store.TurnDone || got.Text != "Hi! Tell me what the workflow should do." {
		t.Fatalf("%+v", got)
	}
	if turns := e.sess.turns(); turns[1].Cmd[slices.Index(turns[1].Cmd, "--resume")+1] != c.ConversationID {
		t.Fatalf("second turn %v", turns[1].Cmd)
	}

	// The log has the messages, the status changes and the events, in order.
	var kinds []string
	for _, en := range e.entries(d.ID) {
		k := en.Type
		if en.Event != nil {
			k += ":" + string(en.Event.Kind)
		}
		if en.Status != "" {
			k += ":" + en.Status
		}
		kinds = append(kinds, k)
	}
	joined := strings.Join(kinds, " ")
	for _, want := range []string{"draft", "message message status:running event:session event:text event:tool_call event:tool_result", "event:done status:done"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log %s\nlacks %s", joined, want)
		}
	}
	// Entries pages by offset and returns whole lines only.
	raw, next, err := e.s.Entries(e.ctx, d.ID, 0)
	if err != nil || len(raw) != len(kinds) {
		t.Fatal(len(raw), err)
	}
	if more, n2, _ := e.s.Entries(e.ctx, d.ID, next); len(more) != 0 || n2 != next {
		t.Fatalf("after the end: %d %d", len(more), n2)
	}
	if again, _, _ := e.s.Entries(e.ctx, d.ID, -5); len(again) != len(raw) {
		t.Fatal("negative offset")
	}
	full, _ := e.s.GetChat(e.ctx, d.ID)
	if full.LogSize != next || len(full.Messages) != 4 || full.SessionStatus != session.StatusRunning {
		t.Fatalf("%+v", full.ChatInfo)
	}
	list, err := e.s.ListChats(e.ctx)
	if err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Fatal(list, err)
	}
}

// Turns of one chat run one after the other, in order.
func TestTurnsAreSerial(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	gate := make(chan struct{})
	started := make(chan struct{})
	e.sess.push(func(ctx context.Context, req session.TurnRequest, out io.Writer) (session.TurnResult, error) {
		close(started)
		<-gate
		return lines(0, `{"type":"result","subtype":"success","is_error":false}`)(ctx, req, out)
	}, lines(0, `{"type":"result","subtype":"success","is_error":false}`))
	a, _ := e.s.Send(e.ctx, d.ID, "one", "local")
	b, _ := e.s.Send(e.ctx, d.ID, "two", "local")
	<-started
	time.Sleep(50 * time.Millisecond)
	if n := len(e.sess.turns()); n != 1 {
		t.Fatalf("%d turns ran at once", n)
	}
	close(gate)
	e.waitMessage(d.ID, a[1].ID)
	e.waitMessage(d.ID, b[1].ID)
	if turns := e.sess.turns(); string(turns[0].Stdin) != "one" || string(turns[1].Stdin) != "two" {
		t.Fatalf("order %q %q", turns[0].Stdin, turns[1].Stdin)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	started := make(chan struct{})
	e.sess.push(blockUntilCancel(started))
	a, _ := e.s.Send(e.ctx, d.ID, "long", "local")
	b, _ := e.s.Send(e.ctx, d.ID, "queued", "local")
	<-started
	if err := e.s.Cancel(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.waitMessage(d.ID, a[1].ID); got.Status != store.TurnCancelled {
		t.Fatalf("running turn %+v", got)
	}
	if got := e.waitMessage(d.ID, b[1].ID); got.Status != store.TurnCancelled || got.Error != "cancelled before it started" {
		t.Fatalf("queued turn %+v", got)
	}
	e.waitFor("the process to be signalled", func() bool {
		n := 0
		e.sess.do(func(st *sessState) { n = len(st.cancels) })
		return n == 1
	})
	if err := e.s.Cancel(e.ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Retry re-sends the user's text.
	e.sess.push(lines(0, `{"type":"result","is_error":false}`))
	r, err := e.s.Retry(e.ctx, d.ID, a[1].ID, "local")
	if err != nil || len(r) != 2 || r[0].Text != "long" {
		t.Fatal(r, err)
	}
	if got := e.waitMessage(d.ID, r[1].ID); got.Status != store.TurnDone {
		t.Fatal(got)
	}
	for _, bad := range []string{a[0].ID, r[1].ID, "nope"} {
		if _, err := e.s.Retry(e.ctx, d.ID, bad, "local"); !errors.Is(err, ErrInvalid) {
			t.Errorf("retry %s: %v", bad, err)
		}
	}
	if _, err := e.s.Retry(e.ctx, "nope", "x", "local"); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
}

func TestTerminalStatuses(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	send := func(s turnScript) MessageInfo {
		t.Helper()
		e.sess.push(s)
		m, err := e.s.Send(e.ctx, d.ID, "go", "local")
		if err != nil {
			t.Fatal(err)
		}
		return e.waitMessage(d.ID, m[1].ID)
	}
	if got := send(fixture(t, "claude", "ratelimit")); got.Status != store.TurnRateLimited || got.ResetAt == nil || got.Error != "Claude AI usage limit reached" {
		t.Errorf("rate limit %+v", got)
	}
	if got := send(fixture(t, "claude", "auth")); got.Status != store.TurnAuthRequired || !strings.Contains(got.Error, "/login") {
		t.Errorf("auth %+v", got)
	}
	if got := send(fixture(t, "claude", "error")); got.Status != store.TurnError || got.Error != "error max turns" {
		t.Errorf("error %+v", got)
	}
	cut := func(ctx context.Context, req session.TurnRequest, out io.Writer) (session.TurnResult, error) {
		_, _ = fixture(t, "claude", "cut")(ctx, req, out)
		return session.TurnResult{ExitCode: 137, Stderr: "a\nb\nc\nd\ne\nf\nkilled"}, nil
	}
	if got := send(cut); got.Status != store.TurnError || got.Text != "Working on it" || !strings.Contains(got.Error, "code 137") || !strings.HasSuffix(got.Error, ": c / d / e / f / killed") {
		t.Errorf("cut %+v", got)
	}
	execErr := func(context.Context, session.TurnRequest, io.Writer) (session.TurnResult, error) {
		return session.TurnResult{}, errors.New("container gone")
	}
	if got := send(execErr); got.Status != store.TurnError || got.Error != "run the agent: container gone" {
		t.Errorf("exec error %+v", got)
	}
	// No retry happened on its own: one exec per message.
	if n := len(e.sess.turns()); n != 5 {
		t.Errorf("%d execs for 5 messages", n)
	}
}

func TestTurnTimeout(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {})
	e.cfg.Assistant.TurnTimeout = 50 * time.Millisecond
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	started := make(chan struct{})
	e.sess.push(blockUntilCancel(started))
	m, _ := e.s.Send(e.ctx, d.ID, "slow", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnError || !strings.Contains(got.Error, "did not finish within 50ms") {
		t.Fatalf("%+v", got)
	}
}

func TestCodexChat(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Profiles = map[string]config.ProfileConfig{"codex": {Agent: "codex", Egress: []string{}, Inject: map[string]string{"openai": "openai-key"}}, "workflow": {Egress: []string{}}}
		c.Sessions.DefaultProfile = "codex"
	})
	d, err := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	if err != nil || d.Agent != "codex" {
		t.Fatal(d, err)
	}
	e.sess.push(fixture(t, "codex", "tools"), lines(0, `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`))
	m, _ := e.s.Send(e.ctx, d.ID, "hi", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnDone || got.Text != "Slack is installed; the draft needs a channel." {
		t.Fatalf("%+v", got)
	}
	m, _ = e.s.Send(e.ctx, d.ID, "again", "local")
	e.waitMessage(d.ID, m[1].ID)
	turns := e.sess.turns()
	if strings.Join(turns[0].Cmd[:2], " ") != "codex exec" || slices.Contains(turns[0].Cmd, "resume") ||
		strings.Join(turns[1].Cmd[:4], " ") != "codex exec resume 0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Fatalf("%v\n%v", turns[0].Cmd, turns[1].Cmd)
	}
	// An API-key profile shows the CLI's cost figure; usage is recorded.
	_, _ = e.st.UpdateChatMessageIf(e.ctx, &store.ChatMessage{ID: m[1].ID, Status: store.TurnDone, CostMicro: 1500, InputTokens: 1}, store.TurnDone)
	full, _ := e.s.GetChat(e.ctx, d.ID)
	if c := full.Messages[3].CostUSD; c == nil || *c != 0.0015 {
		t.Fatalf("cost %v", c)
	}
}

func TestSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	ok := `{"type":"result","is_error":false}`
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")

	// Queued behind the cap, then running: the turn waits and says why.
	e.sess.do(func(st *sessState) { st.queuedPolls = 3 })
	e.sess.push(lines(0, ok))
	m, _ := e.s.Send(e.ctx, d.ID, "hi", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnDone {
		t.Fatal(got)
	}
	found := false
	for _, en := range e.entries(d.ID) {
		found = found || strings.Contains(en.Note, "Waiting for a free assistant slot")
	}
	if !found {
		t.Error("no queued note")
	}

	// A stopped session (the idle timeout) is started again.
	c, _ := e.st.GetChat(e.ctx, d.ID)
	_, _ = e.sess.Stop(e.ctx, c.SessionID)
	e.sess.push(lines(0, ok))
	m, _ = e.s.Send(e.ctx, d.ID, "back", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnDone {
		t.Fatal(got)
	}

	// A session that is gone is replaced.
	_ = e.sess.Remove(e.ctx, c.SessionID, false)
	e.sess.push(lines(0, ok))
	m, _ = e.s.Send(e.ctx, d.ID, "new", "local")
	e.waitMessage(d.ID, m[1].ID)
	if c2, _ := e.st.GetChat(e.ctx, d.ID); c2.SessionID == c.SessionID || c2.SessionID == "" {
		t.Fatalf("session %s -> %s", c.SessionID, c2.SessionID)
	}

	// Failures to get a session fail the turn with the reason.
	c, _ = e.st.GetChat(e.ctx, d.ID)
	_, _ = e.sess.Stop(e.ctx, c.SessionID)
	e.sess.do(func(st *sessState) { st.startErr = errors.New("docker is down") })
	m, _ = e.s.Send(e.ctx, d.ID, "x", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnError || !strings.Contains(got.Error, "docker is down") {
		t.Fatal(got)
	}
	_ = e.sess.Remove(e.ctx, c.SessionID, false)
	e.sess.do(func(st *sessState) { st.createErr = errors.New("no image") })
	m, _ = e.s.Send(e.ctx, d.ID, "x", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnError || !strings.Contains(got.Error, "no image") {
		t.Fatal(got)
	}
	e.sess.do(func(st *sessState) { st.createErr, st.queuedPolls, st.failStart = nil, 1, true })
	m, _ = e.s.Send(e.ctx, d.ID, "x", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnError || !strings.Contains(got.Error, "image pull failed") {
		t.Fatal(got)
	}
}

func TestIdleStop(t *testing.T) {
	e := newEnv(t)
	e.cfg.Assistant.IdleTimeout = 20 * time.Millisecond
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.push(lines(0, `{"type":"result","is_error":false}`))
	m, _ := e.s.Send(e.ctx, d.ID, "hi", "local")
	e.waitMessage(d.ID, m[1].ID)
	e.waitFor("the idle stop", func() bool {
		n := 0
		e.sess.do(func(st *sessState) { n = len(st.stops) })
		return n == 1
	})
	e.waitFor("the idle note", func() bool {
		for _, en := range e.entries(d.ID) {
			if strings.Contains(en.Note, "quiet period") {
				return true
			}
		}
		return false
	})
}

func TestRedaction(t *testing.T) {
	e := newEnv(t)
	if err := e.sec.Set(e.ctx, "slack-token", "s3cr3t-value-123", nil); err != nil {
		t.Fatal(err)
	}
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.push(lines(0,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"the token is s3cr3t-value-123 and ghp_abcdefghijklmnopqrstuvwxyz0123"},{"type":"tool_use","id":"t","name":"mcp__icaro__validate_workflow","input":{"yaml":"token: s3cr3t-value-123"}}]}}`,
		`{"type":"result","is_error":false}`))
	m, _ := e.s.Send(e.ctx, d.ID, "my key is s3cr3t-value-123", "local")
	got := e.waitMessage(d.ID, m[1].ID)
	full, _ := e.s.GetChat(e.ctx, d.ID)
	raw, err := os.ReadFile(e.s.logPath(d.ID))
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{"reply": got.Text, "user message": full.Messages[0].Text, "log": string(raw)} {
		if strings.Contains(s, "s3cr3t-value-123") || strings.Contains(s, "ghp_abcdefghij") {
			t.Errorf("%s leaks a secret: %s", what, s)
		}
	}
	// The turn itself got the person's real text.
	if string(e.sess.turns()[0].Stdin) != "my key is s3cr3t-value-123" {
		t.Error("stdin was redacted")
	}
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.CreateChat(e.ctx, CreateRequest{ProjectID: "nope"}, "local"); !errors.Is(err, ErrInvalid) {
		t.Error(err)
	}
	if _, err := e.s.CreateChat(e.ctx, CreateRequest{Workflow: "nope"}, "local"); !errors.Is(err, ErrInvalid) {
		t.Error(err)
	}
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{Title: strings.Repeat("t", 200)}, "local")
	if len([]rune(d.Title)) != 120 || !strings.HasSuffix(d.Title, "…") {
		t.Errorf("title %q", d.Title)
	}
	for _, text := range []string{" ", strings.Repeat("x", MaxMessage+1), "\xff"} {
		if _, err := e.s.Send(e.ctx, d.ID, text, "local"); !errors.Is(err, ErrInvalid) {
			t.Errorf("send %.10q: %v", text, err)
		}
	}
	if _, err := e.s.Send(e.ctx, "nope", "hi", "local"); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	if _, err := e.s.GetChat(e.ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	if _, err := e.s.PutDraft(e.ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	if _, err := e.s.PutDraft(e.ctx, d.ID, strings.Repeat("x", maxDraft+1)); !errors.Is(err, ErrInvalid) {
		t.Error(err)
	}
	if _, _, err := e.s.Entries(e.ctx, "nope", 0); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	if raw, n, err := e.s.Entries(e.ctx, d.ID, 0); err != nil || raw != nil || n != 0 {
		t.Error("a chat without a log", raw, n, err)
	}
	e.cfg.Profiles = map[string]config.ProfileConfig{"sh": {}}
	if _, err := e.s.CreateChat(e.ctx, CreateRequest{}, "local"); !errors.Is(err, ErrInvalid) {
		t.Error(err)
	}
	// A chat whose profile lost its chat-capable agent fails its turn.
	e.cfg.Profiles = map[string]config.ProfileConfig{"claude": {}}
	m, _ := e.s.Send(e.ctx, d.ID, "hi", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnError || !strings.Contains(got.Error, "cannot run the assistant") {
		t.Errorf("%+v", got)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{YAML: helloYAML}, "local")
	started := make(chan struct{})
	e.sess.push(blockUntilCancel(started))
	_, _ = e.s.Send(e.ctx, d.ID, "hi", "local")
	<-started
	c, _ := e.st.GetChat(e.ctx, d.ID)
	if err := e.s.Delete(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetChat(e.ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.s.logPath(d.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("log kept")
	}
	var removed []string
	e.sess.do(func(st *sessState) { removed = st.removed })
	if !slices.Contains(removed, c.SessionID) {
		t.Fatalf("session kept: %v", removed)
	}
	if err := e.s.Delete(e.ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// A chat without a session deletes too.
	d2, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	if err := e.s.Delete(e.ctx, d2.ID); err != nil {
		t.Fatal(err)
	}
}

// A restart interrupts lost turns, expires their approvals and signals
// leftover processes.
func TestRestartRecovery(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	sess, _ := e.sess.CreateAssist(e.ctx, session.AssistRequest{ChatID: d.ID})
	_ = e.st.SetChatField(e.ctx, d.ID, store.ChatSessionID, sess.ID)
	m := &store.ChatMessage{ChatID: d.ID, Role: store.RoleAssistant, Status: store.TurnRunning}
	_ = e.st.AddChatMessage(e.ctx, m)
	ap := &store.ChatApproval{ChatID: d.ID, Tool: ToolRun, Request: "{}", ExpiresAt: time.Now().Add(time.Hour)}
	_ = e.st.CreateChatApproval(e.ctx, ap)
	other := &store.Chat{Profile: "claude", Agent: "claude"}
	_ = e.st.CreateChat(e.ctx, other)

	s2 := New(Options{Store: e.st, Config: e.cfg, Sessions: e.sess, Workflows: e.svc, LogsDir: t.TempDir(), Log: e.s.o.Log})
	ctx, cancel := context.WithCancel(e.ctx)
	go func() { _ = s2.Run(ctx) }()
	e.waitFor("the turn to be interrupted", func() bool {
		got, _ := e.st.GetChatMessage(e.ctx, m.ID)
		return got.Status == store.TurnInterrupted
	})
	if got, _ := e.st.GetChatApproval(e.ctx, ap.ID); got.Decision != DecisionExpired {
		t.Fatalf("%+v", got)
	}
	e.waitFor("the leftover to be signalled", func() bool {
		var c []string
		e.sess.do(func(st *sessState) { c = st.cancels })
		return slices.Contains(c, sess.ID)
	})
	cancel()
}

func TestHelpers(t *testing.T) {
	if lastLines("a\nb\nc", 2) != "b / c" || firstLine("a\nb") != "a" || clip("abc", 5) != "abc" {
		t.Fatal("helpers")
	}
	u := newUUID()
	if len(u) != 36 || u[14] != '4' || !strings.ContainsAny(u[19:20], "89ab") {
		t.Fatal(u)
	}
	if New(Options{}).Name() != "assistant" {
		t.Fatal("name")
	}
	var r redactor
	if string(r.Redact([]byte("x"))) != "x" {
		t.Fatal("empty redactor")
	}
	_ = json.Valid
}
