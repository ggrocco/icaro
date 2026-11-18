package assist

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icaro/internal/assist/distill"
	"icaro/internal/memory"
	"icaro/internal/service"
	"icaro/internal/store"
)

// validateCall streams one validate_workflow call and its result.
func validateCall(id, yaml, result string) []string {
	args, _ := json.Marshal(map[string]string{"yaml": yaml})
	res, _ := json.Marshal(result)
	return []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"mcp__icaro__validate_workflow","input":` + string(args) + `}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + string(res) + `}]}}`,
	}
}

// brokenYAML is helloYAML with one field misspelled.
var brokenYAML = strings.Replace(helloYAML, "    image:", "    imagex:", 1)

// fakeModel answers Open with a scripted completer.
type fakeModel struct {
	c    distill.Completer
	skip string
	err  error
	// The distiller's goroutine counts; the test reads.
	opens  atomic.Int32
	closes atomic.Int32
}

func (f *fakeModel) Open(context.Context) (distill.Completer, func(), string, error) {
	f.opens.Add(1)
	if f.err != nil {
		return nil, nil, "", f.err
	}
	return f.c, func() { f.closes.Add(1) }, f.skip, nil
}

type scripted struct {
	out string
	err error
	n   int
}

func (s *scripted) Complete(context.Context, string, string, json.RawMessage, int) (string, error) {
	s.n++
	return s.out, s.err
}

const summaryJSON = `{"intent":"Greet whoever the trigger names","constraints":["only on weekdays"],"preferences":["alpine images"],
"rejected":[{"what":"a busybox image","why":"they prefer alpine"}],"open":["which channel?"],"resume":"The hello workflow is saved; next, add a Slack step."}`

// runDistill processes the chat's job now and returns it.
func (e *env) runDistill(d *Distiller, chatID string) *store.DistillJob {
	e.t.Helper()
	if err := d.Enqueue(e.ctx, chatID); err != nil {
		e.t.Fatal(err)
	}
	job, err := e.st.ClaimDistill(e.ctx, time.Now().Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	var b batch
	defer b.done()
	d.process(e.ctx, job, &b)
	got, err := e.st.GetDistill(e.ctx, chatID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *env) produced(chatID string) map[string]*memory.Memory {
	e.t.Helper()
	m, err := e.s.Memories(e.ctx, chatID)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]*memory.Memory{}
	for _, p := range m.Produced {
		typ, _ := p.Metadata["type"].(string)
		out[typ] = p
	}
	return out
}

// A chat that fixed a validator issue, saved its workflow and ran it
// leaves exact memories and its stats; with a model, a summary too.
func TestDistill(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{YAML: helloYAML}, "local")
	ls := append([]string{`{"type":"system","subtype":"init","session_id":"c1"}`},
		validateCall("v1", brokenYAML, `{"valid": false, "issues": [{"path": "/steps/0/imagex", "message": "unknown field \"imagex\"", "hint": "did you mean image?"}]}`)...)
	ls = append(ls, validateCall("v2", helloYAML, `{"valid": true, "issues": []}`)...)
	ls = append(ls, `{"type":"assistant","message":{"content":[{"type":"text","text":"Fixed the image field."}]}}`, `{"type":"result","is_error":false}`)
	e.sess.push(lines(0, ls...))
	m, _ := e.s.Send(e.ctx, d.ID, "make it say hello", "local")
	e.waitMessage(d.ID, m[1].ID)
	if _, err := e.s.Apply(e.ctx, d.ID, helloYAML, "token:alice"); err != nil {
		t.Fatal(err)
	}
	// A run the person approved, which succeeds.
	_, sid := e.chatWithSessionFor(d.ID)
	done := make(chan map[string]any, 1)
	go func() {
		out, _ := e.tool(sid, ToolRun, map[string]any{"name": "hello", "trigger": map[string]any{"who": "x"}})
		done <- out
	}()
	e.waitFor("an approval", func() bool { full, _ := e.s.GetChat(e.ctx, d.ID); return len(full.Approvals) == 1 })
	full, _ := e.s.GetChat(e.ctx, d.ID)
	if _, err := e.s.Decide(e.ctx, d.ID, full.Approvals[0].ID, true, "token:alice"); err != nil {
		t.Fatal(err)
	}
	out := <-done
	e.waitFor("the run to finish", func() bool {
		r, err := e.svc.GetRun(e.ctx, out["run_id"].(string))
		if err == nil && service.Terminal(r.Status) {
			if r.Status != "succeeded" {
				t.Fatalf("run %s: %s %+v", r.Status, r.Error, r.Steps)
			}
			return true
		}
		return false
	})

	model := &fakeModel{c: &scripted{out: summaryJSON}}
	dist := NewDistiller(e.s, e.svc, model)
	job := e.runDistill(dist, d.ID)
	if job.Status != store.DistillDone || job.Stage2 != "done" || job.Memories < 8 {
		t.Fatalf("%+v", job)
	}
	got := e.produced(d.ID)
	lesson := got["validator_lesson"]
	if lesson == nil || !strings.Contains(lesson.Title, `unknown field "imagex" at /steps/0/imagex`) || !strings.Contains(lesson.Body, "-     imagex: alpine") || !strings.Contains(lesson.Body, "+     image: alpine") ||
		!strings.Contains(lesson.Body, "Hint: did you mean image?") || lesson.Metadata["confidence"] != ConfidenceExact || lesson.ExpiresAt != nil {
		t.Fatalf("lesson %+v", lesson)
	}
	if r := got["run_outcome"]; r == nil || !strings.Contains(r.Title, "hello v1 succeeded") {
		t.Fatalf("run %+v", r)
	}
	if s := got["saved"]; s == nil || s.Kind != memory.KindDecision || !strings.Contains(s.Body, "token:alice saved hello v1") {
		t.Fatalf("saved %+v", s)
	}
	if ex := got["working_example"]; ex == nil || !strings.Contains(ex.Body, "name: hello") {
		t.Fatalf("example %+v", ex)
	}
	intent := got["intent"]
	if intent == nil || intent.Metadata["confidence"] != ConfidenceModel || intent.ExpiresAt == nil || !strings.Contains(intent.Title, "Greet whoever") {
		t.Fatalf("intent %+v", intent)
	}
	for _, typ := range []string{"constraints", "preferences", "rejected", "open", "resume"} {
		if got[typ] == nil {
			t.Errorf("no %s memory", typ)
		}
	}
	if got["open"].Kind != memory.KindTask || got["rejected"].Kind != memory.KindDecision || got["resume"].Kind != memory.KindFinding {
		t.Error("kinds")
	}
	stats, _ := e.st.ListChatStats(e.ctx)
	if len(stats) != 1 || stats[0].Validations != 2 || stats[0].Saves != 1 || stats[0].TurnsToSave != 1 || stats[0].Turns != 1 || stats[0].UsedMemories != 0 {
		t.Fatalf("stats %+v", stats[0])
	}

	// The workflow's page shows the latest intent and resume.
	notes, err := e.s.WorkflowNotes(e.ctx, "hello")
	if err != nil || len(notes) != 2 || notes[0].Type != "intent" || notes[1].Type != "resume" || notes[0].ChatID != d.ID {
		t.Fatalf("%+v %v", notes, err)
	}
	if n, _ := e.s.WorkflowNotes(e.ctx, ""); len(n) != 0 {
		t.Fatal("notes for no workflow")
	}

	// Running it again changes nothing and opens the model once per batch.
	if again := e.runDistill(dist, d.ID); again.Memories != job.Memories {
		t.Fatalf("not idempotent: %d then %d", job.Memories, again.Memories)
	}

	// A memory the person removed is not written again.
	if err := e.s.RemoveMemory(e.ctx, d.ID, lesson.ID); err != nil {
		t.Fatal(err)
	}
	e.runDistill(dist, d.ID)
	if e.produced(d.ID)["validator_lesson"] != nil {
		t.Fatal("a removed memory came back")
	}
	if err := e.s.RemoveMemory(e.ctx, d.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// The next chat on the workflow starts with the notes.
	d2, _ := e.s.CreateChat(e.ctx, CreateRequest{Workflow: "hello"}, "local")
	e.sess.push(lines(0, `{"type":"system","subtype":"init","session_id":"conv-2"}`, `{"type":"result","is_error":false}`), lines(0, `{"type":"result","is_error":false}`))
	m2, _ := e.s.Send(e.ctx, d2.ID, "hello workflow: add a Slack step", "local")
	e.waitMessage(d2.ID, m2[1].ID)
	turns := e.sess.turns()
	first := string(turns[len(turns)-1].Stdin)
	if !strings.HasPrefix(first, "[Notes from earlier chats") || !strings.Contains(first, "Greet whoever") || !strings.HasSuffix(first, "---\n\nhello workflow: add a Slack step") {
		t.Fatalf("first turn stdin:\n%s", first)
	}
	// The lesson the person removed is in neither the notes nor the
	// common mistakes.
	if strings.Contains(first, "imagex") {
		t.Fatalf("a removed memory reached the preamble:\n%s", first)
	}
	mems, _ := e.s.Memories(e.ctx, d2.ID)
	if len(mems.Used) == 0 {
		t.Fatal("used memories not logged")
	}
	// Only the first turn of a conversation gets them.
	m3, _ := e.s.Send(e.ctx, d2.ID, "and one more thing", "local")
	e.waitMessage(d2.ID, m3[1].ID)
	turns = e.sess.turns()
	if got := string(turns[len(turns)-1].Stdin); got != "and one more thing" {
		t.Fatalf("second turn %q", got)
	}
	e.runDistill(dist, d2.ID)
	st, err := e.s.Stats(e.ctx)
	if err != nil || st.WithMemory.Chats != 1 || st.WithoutMemory.Chats != 1 || st.WithoutMemory.Saved != 1 || st.WithoutMemory.AvgValidations != 2 {
		t.Fatalf("%+v %v", st, err)
	}
}

func (e *env) chatWithSessionFor(chatID string) (*store.Chat, string) {
	e.t.Helper()
	c, _ := e.st.GetChat(e.ctx, chatID)
	return c, c.SessionID
}

func TestDistillStage2Outcomes(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.push(lines(0, `{"type":"assistant","message":{"content":[{"type":"text","text":"Hi."}]}}`, `{"type":"result","is_error":false}`))
	m, _ := e.s.Send(e.ctx, d.ID, "hello", "local")
	e.waitMessage(d.ID, m[1].ID)

	// No model: stage 2 is skipped with the reason.
	skip := NewDistiller(e.s, e.svc, &fakeModel{skip: "no model pulled"})
	if j := e.runDistill(skip, d.ID); j.Status != store.DistillDone || j.Stage2 != "skipped: no model pulled" {
		t.Fatalf("%+v", j)
	}
	if j := e.runDistill(NewDistiller(e.s, e.svc, nil), d.ID); j.Stage2 != "skipped: no model" {
		t.Fatalf("%+v", j)
	}
	// The model fails: retried once, then failed; stage 1 still ran.
	bad := NewDistiller(e.s, e.svc, &fakeModel{c: &scripted{err: errors.New("server gone")}})
	j := e.runDistill(bad, d.ID)
	if j.Status != store.DistillPending || !strings.Contains(j.Error, "server gone") || !strings.HasPrefix(j.Stage2, "failed: ") {
		t.Fatalf("first failure %+v", j)
	}
	job, _ := e.st.ClaimDistill(e.ctx, time.Now().Add(time.Hour))
	var b batch
	bad.process(e.ctx, job, &b)
	if j, _ := e.st.GetDistill(e.ctx, d.ID); j.Status != store.DistillFailed {
		t.Fatalf("second failure %+v", j)
	}
	// The model server cannot open.
	open := NewDistiller(e.s, e.svc, &fakeModel{err: errors.New("no docker")})
	if j := e.runDistill(open, d.ID); !strings.Contains(j.Error, "no docker") {
		t.Fatalf("%+v", j)
	}
	// Invalid model output.
	junk := NewDistiller(e.s, e.svc, &fakeModel{c: &scripted{out: "not json"}})
	if j := e.runDistill(junk, d.ID); !strings.Contains(j.Error, "not a valid summary") {
		t.Fatalf("%+v", j)
	}
	// A chat with nothing to summarize.
	empty, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	if j := e.runDistill(NewDistiller(e.s, e.svc, &fakeModel{c: &scripted{out: summaryJSON}}), empty.ID); j.Stage2 != "skipped: nothing to summarize" {
		t.Fatalf("%+v", j)
	}
	// A chat that is gone fails without a retry.
	if _, err := e.st.EnqueueDistill(e.ctx, "ghost", time.Now()); err != nil {
		t.Fatal(err)
	}
	job, _ = e.st.ClaimDistill(e.ctx, time.Now().Add(time.Hour))
	skip.process(e.ctx, job, &b)
	if j, _ := e.st.GetDistill(e.ctx, "ghost"); j.Status != store.DistillFailed {
		t.Fatalf("%+v", j)
	}
}

func TestDistillerLoopAndIdle(t *testing.T) {
	e := newEnv(t)
	e.cfg.Assistant.IdleTimeout = 20 * time.Millisecond
	model := &fakeModel{c: &scripted{out: summaryJSON}}
	dist := NewDistiller(e.s, e.svc, model)
	dist.Poll = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go func() { _ = dist.Run(ctx) }()
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.push(lines(0, `{"type":"assistant","message":{"content":[{"type":"text","text":"Hi."}]}}`, `{"type":"result","is_error":false}`))
	m, _ := e.s.Send(e.ctx, d.ID, "hello there", "local")
	e.waitMessage(d.ID, m[1].ID)
	// The chat goes idle, is queued, and the loop distills it, then stops
	// the model server when the queue is empty.
	e.waitFor("the idle chat to be distilled", func() bool {
		j, err := e.st.GetDistill(e.ctx, d.ID)
		return err == nil && j.Status == store.DistillDone
	})
	e.waitFor("the model server to stop", func() bool { return model.closes.Load() == model.opens.Load() && model.opens.Load() > 0 })
	if err := e.s.Distill(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Distill(e.ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Delete drops the job but keeps the memories.
	if err := e.s.Delete(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetDistill(e.ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("job kept")
	}
	if got, _ := e.mem.ByOrigin(e.ctx, memory.Scope{ProjectID: GlobalProject}, Origin(d.ID), 10); len(got) == 0 {
		t.Fatal("memories were deleted with the chat")
	}
}

func TestNoDistiller(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	if err := e.s.Distill(e.ctx, d.ID); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	e.s.o.Memories = nil
	if err := e.s.RemoveMemory(e.ctx, d.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if notes, refs := e.s.recall(e.ctx, &store.Chat{}, "x"); notes != "" || refs != nil {
		t.Fatal("recall without memory")
	}
	m, err := e.s.Memories(e.ctx, d.ID)
	if err != nil || len(m.Produced) != 0 {
		t.Fatal(m, err)
	}
	if j := e.runDistill(NewDistiller(e.s, e.svc, nil), d.ID); j.Stage2 != "skipped: no memory engine" {
		t.Fatalf("%+v", j)
	}
}

func TestStage1Helpers(t *testing.T) {
	// A cut-short result is still read.
	ch := parseCheck("y", `{"valid": false, "issues": [{"path": "/a", "message": "bad \"x\""}, {"path": "/b", "mes`, false)
	if !ch.known || ch.valid || len(ch.issues) != 1 || ch.issues[0].Message != `bad "x"` {
		t.Fatalf("%+v", ch)
	}
	if ch := parseCheck("y", "boom", true); ch.known {
		t.Fatal("an error result is not a check")
	}
	if unescape(`bad \q`) != `bad \q` {
		t.Fatal("unescape fallback")
	}
	if d := changedLines("a\nb\nc", "a\nB\nc\nd", 10); d != "+ B\n- b\n+ d" && d != "- b\n+ B\n+ d" {
		t.Fatalf("%q", d)
	}
	if d := changedLines("a\nb\nc", "x\ny\nz", 2); strings.Count(d, "\n") != 1 {
		t.Fatalf("limit: %q", d)
	}
	if changedLines(strings.Repeat("a\n", 1100), strings.Repeat("b\n", 1100), 5) != "" {
		t.Fatal("too big")
	}
	facts := integrationFacts("bugs", `version: 1
name: bugs
triggers:
  - integration: github
    event: issues.labeled
steps:
  - id: post
    uses: slack.post-message
    input: {channel: "#ops", text: "hi"}
    env: {TOKEN: "${{ secrets.slack-bot }}"}
  - id: plain
    image: alpine
`)
	if len(facts) != 2 || !strings.Contains(facts[0].Body, "inputs: channel, text") || !strings.Contains(facts[0].Body, "secrets slack-bot") ||
		!strings.Contains(facts[1].Title, "github.issues.labeled") {
		t.Fatalf("%+v", facts)
	}
	if integrationFacts("x", "[not: yaml") != nil {
		t.Fatal("bad yaml")
	}
	if clipBytes("héllo", 2) != "h\n# … (cut)" || orNone(nil) != "none" || nameOr(" ", "d") != "d" {
		t.Fatal("helpers")
	}
	mk := commonMistakes([]*memory.Memory{
		{Metadata: map[string]any{"message": "a", "path": "/x"}}, {Metadata: map[string]any{"message": "a"}},
		{Metadata: map[string]any{"message": "b"}}, {Metadata: map[string]any{}},
	}, 5)
	if len(mk) != 2 || mk[0] != "- a (e.g. at /x), 2 time(s)" {
		t.Fatalf("%v", mk)
	}
	g := group([]*store.ChatStats{{Validations: 2, Saves: 1, TurnsToSave: 2, SecondsToSave: 30, Proposals: 2}, {Validations: -1, TurnsToSave: -1}})
	if g.Chats != 2 || g.Converged != 1 || g.Saved != 1 || g.AcceptRate != 0.5 || g.MedianSecondsToSave != 30 {
		t.Fatalf("%+v", g)
	}
	var _ = service.Terminal
}

func TestRecallBudget(t *testing.T) {
	e := newEnv(t)
	scope := memory.Scope{ProjectID: GlobalProject}
	var items []memory.ImportItem
	for i := range 30 {
		items = append(items, memory.ImportItem{Key: "k" + string(rune('a'+i)), Kind: memory.KindFinding, Title: "Integration slack note " + string(rune('a'+i)),
			Body: strings.Repeat("slack detail ", 40), Source: SourceFact})
	}
	items = append(items, memory.ImportItem{Key: "lesson", Kind: memory.KindFinding, Title: "Validator: x", Source: SourceLesson, Metadata: map[string]any{"message": "unknown field", "path": "/steps/0"}})
	if _, err := e.mem.Put(e.ctx, scope, "distill:old", items); err != nil {
		t.Fatal(err)
	}
	e.cfg.Assistant.MemoryBudget = 200 // 800 characters
	notes, refs := e.s.recall(e.ctx, &store.Chat{Workflow: "slack"}, "slack")
	if len(notes) > 800 || len(refs) == 0 || len(refs) > 3 {
		t.Fatalf("%d chars, %d refs", len(notes), len(refs))
	}
	e.cfg.Assistant.MemoryBudget = 0
	if notes, _ := e.s.recall(e.ctx, &store.Chat{}, "slack"); notes != "" {
		t.Fatal("budget 0")
	}
	e.cfg.Assistant.MemoryBudget = 2000
	notes, _ = e.s.recall(e.ctx, &store.Chat{}, "nothing matches zzz")
	if !strings.Contains(notes, "unknown field (e.g. at /steps/0), 1 time(s)") {
		t.Fatalf("mistakes: %s", notes)
	}
}
