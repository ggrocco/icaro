package assist

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"icaro/internal/assist/stream"
	"icaro/internal/memory"
	"icaro/internal/service"
	"icaro/internal/store"
)

// failingMemories fails every call.
type failingMemories struct{ err error }

func (f failingMemories) Put(context.Context, memory.Scope, string, []memory.ImportItem) (*memory.ImportReport, error) {
	return nil, f.err
}
func (f failingMemories) ByOrigin(context.Context, memory.Scope, string, int) ([]*memory.Memory, error) {
	return nil, f.err
}
func (f failingMemories) Query(context.Context, memory.Scope, memory.Query) ([]*memory.Memory, error) {
	return nil, f.err
}
func (f failingMemories) Remove(context.Context, memory.Scope, ...string) error { return f.err }

func TestMemoryEngineFailures(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("memory down")
	e.s.o.Memories = failingMemories{boom}
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{YAML: helloYAML}, "local")
	if _, err := e.s.Memories(e.ctx, d.ID); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := e.s.RemoveMemory(e.ctx, d.ID, "m"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if notes, _ := e.s.recall(e.ctx, &store.Chat{}, "x"); notes != "" {
		t.Fatal("notes from a failing engine")
	}
	// A lesson to write makes Put fail: the job retries.
	e.sess.push(lines(0, append(append([]string{}, validateCall("a", brokenYAML, `{"valid":false,"issues":[{"path":"/x","message":"m"}]}`)...),
		append(validateCall("b", helloYAML, `{"valid":true}`), `{"type":"result","is_error":false}`)...)...))
	m, _ := e.s.Send(e.ctx, d.ID, "go", "local")
	e.waitMessage(d.ID, m[1].ID)
	j := e.runDistill(NewDistiller(e.s, e.svc, nil), d.ID)
	if j.Status != store.DistillPending || !strings.Contains(j.Error, "memory down") {
		t.Fatalf("%+v", j)
	}
	// With the model failing too, stage 1's write error wins.
	j = e.runDistill(NewDistiller(e.s, e.svc, &fakeModel{c: &scripted{err: errors.New("model down")}}), d.ID)
	if !strings.Contains(j.Error, "memory down") {
		t.Fatalf("%+v", j)
	}
}

func TestDistillStoreFailures(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	dist := NewDistiller(e.s, e.svc, nil)
	if dist.Name() == "" {
		t.Fatal("name")
	}
	_ = e.st.Close()
	if err := dist.Enqueue(e.ctx, d.ID); err == nil {
		t.Fatal("enqueue on a closed store")
	}
	if _, err := e.s.Stats(e.ctx); err == nil {
		t.Fatal("stats on a closed store")
	}
	// The loop survives a broken store and stops with its context.
	dist.Poll = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(e.ctx, 50*time.Millisecond)
	defer cancel()
	if err := dist.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dist.distill(e.ctx, d.ID, &batch{}); err == nil {
		t.Fatal("distill on a closed store")
	}
}

func TestDistillerResetsInterruptedJobs(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	if _, err := e.st.EnqueueDistill(e.ctx, d.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.ClaimDistill(e.ctx, time.Now()); err != nil { // running when the daemon "died"
		t.Fatal(err)
	}
	dist := NewDistiller(e.s, e.svc, nil)
	dist.Poll = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go func() { _ = dist.Run(ctx) }()
	e.waitFor("the interrupted job to finish", func() bool {
		j, err := e.st.GetDistill(e.ctx, d.ID)
		return err == nil && j.Status == store.DistillDone
	})
}

// fakeRuns scripts the runs and versions stage 1 reads.
type fakeRuns struct {
	runs     map[string]*service.RunInfo
	versions []service.VersionInfo
	vErr     error
	ok       []*service.RunInfo
	yamlErr  error
}

func (f fakeRuns) GetRun(_ context.Context, id string) (*service.RunInfo, error) {
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, service.ErrNotFound
}
func (f fakeRuns) ListRuns(context.Context, string, string, int) ([]*service.RunInfo, error) {
	return f.ok, nil
}
func (f fakeRuns) ListWorkflowVersions(context.Context, string) ([]service.VersionInfo, error) {
	return f.versions, f.vErr
}
func (f fakeRuns) GetWorkflowVersion(context.Context, string, int) (*service.VersionInfo, error) {
	if f.yamlErr != nil {
		return nil, f.yamlErr
	}
	return &service.VersionInfo{YAML: "name: w"}, nil
}

func TestOutcomes(t *testing.T) {
	ctx := context.Background()
	rx := &redactor{values: []string{"s3cr3t-value"}}
	c := &store.Chat{ID: "c1", Workflow: "w"}
	r := fakeRuns{
		runs: map[string]*service.RunInfo{
			"failed":  {ID: "failed", Workflow: "w", Version: 1, Status: "failed", Steps: []service.StepInfo{{ID: "ok", Status: "succeeded"}, {ID: "post", Status: "failed", Error: "401 with s3cr3t-value"}}},
			"running": {ID: "running", Workflow: "w", Version: 1, Status: "running"},
		},
		versions: []service.VersionInfo{{Version: 1, Actor: "token:a via assistant c1"}, {Version: 2, Actor: "token:a"}, {Version: 3, Actor: "token:a via assistant c1"}},
		ok:       []*service.RunInfo{{Version: 2}, {Version: 3}},
	}
	aps := []*store.ChatApproval{{Result: "failed"}, {Result: "running"}, {Result: "missing"}, {Result: ""}}
	items := outcomes(ctx, r, c, aps, rx)
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	joined := strings.Join(titles, " | ")
	for _, want := range []string{"Run of w v1 failed, step post failed", "Saved w v1 from", "Saved w v3 from", "Working example: w v3 ran green"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "running") || strings.Contains(joined, "w v2") || strings.Contains(items[0].Body, "s3cr3t") {
		t.Fatalf("%s / %s", joined, items[0].Body)
	}
	// A version list that fails, and a YAML that cannot be read.
	r.vErr = errors.New("gone")
	if got := outcomes(ctx, r, c, aps, rx); len(got) != 1 {
		t.Fatalf("%d", len(got))
	}
	r.vErr, r.yamlErr = nil, errors.New("gone")
	for _, it := range outcomes(ctx, r, c, nil, rx) {
		if strings.HasPrefix(it.Title, "Working example") {
			t.Fatal("an example without its YAML")
		}
	}
	if got := outcomes(ctx, r, &store.Chat{ID: "c1"}, nil, rx); got != nil {
		t.Fatal("no workflow, no saves")
	}
}

func TestReadFactsAndLessons(t *testing.T) {
	c := &store.Chat{CreatedAt: time.Now().Add(-time.Minute)}
	ev := func(kind stream.Kind, server, tool, id, args, result string) entry {
		e := entry{Type: EntryEvent, Event: &stream.Event{Kind: kind, Server: server, Tool: tool, ToolID: id, Result: result}}
		if args != "" {
			e.Event.Args = []byte(args)
		}
		return e
	}
	entries := []entry{
		{Type: EntryMemory, Memories: []MemoryRef{{ID: "a"}, {ID: "b"}}},
		ev(stream.KindToolCall, "other", "validate_workflow", "x", `{"yaml":"a"}`, ""),
		ev(stream.KindToolResult, "icaro", "validate_workflow", "never-called", "", `{"valid":true}`),
		ev(stream.KindToolCall, "icaro", "propose_workflow", "p1", `{"yaml":"a: 1"}`, ""),
		ev(stream.KindToolResult, "icaro", "propose_workflow", "p1", "", `{"valid":false,"issues":[{"path":"/a","message":"m"},{"path":"/a","message":"m"}]}`),
		ev(stream.KindToolCall, "icaro", "validate_workflow", "v1", `{"yaml":"a: 2"}`, ""),
		ev(stream.KindToolResult, "icaro", "validate_workflow", "v1", "", `{"valid":false,"issues":[{"path":"/b","message":"n"}]}`),
		ev(stream.KindToolCall, "icaro", "validate_workflow", "v2", `not json`, ""),
		ev(stream.KindToolResult, "icaro", "validate_workflow", "v2", "", `{"valid":true}`),
		{Type: EntryStatus, MessageID: "m1", Status: store.TurnDone},
		{Type: EntryStatus, Note: "Saved as w v1.", Time: time.Now()},
		{Type: EntryStatus, Note: "Saved as w v2.", Time: time.Now()},
		ev(stream.KindToolCall, "icaro", "validate_workflow", "v3", `{"yaml":"a: 3"}`, ""),
		ev(stream.KindToolResult, "icaro", "validate_workflow", "v3", "", `cut sh`),
	}
	f := readFacts(c, entries)
	st := f.stats("c")
	if st.UsedMemories != 2 || st.Turns != 1 || st.Proposals != 1 || st.Saves != 2 || st.TurnsToSave != 1 || st.SecondsToSave < 59 || st.Validations != 3 {
		t.Fatalf("%+v", st)
	}
	ls := f.lessons("w")
	// Only the last failing check before the pass counts, without a diff
	// since the passing YAML could not be read.
	if len(ls) != 1 || !strings.Contains(ls[0].Title, "n at /b") || strings.Contains(ls[0].Body, "```diff") {
		t.Fatalf("%+v", ls)
	}
	if (facts{}).stats("x").Validations != -1 {
		t.Fatal("no checks")
	}
}
