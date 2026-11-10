package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite the .golden files")

// TestFixtures parses every recorded or modeled stream under
// testdata/<agent>/<version>/ and compares the events with its .golden
// file. Spike S11 adds directories of real recordings beside "modeled".
func TestFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/*/*/*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			agent := strings.Split(filepath.ToSlash(f), "/")[1]
			p, err := New(agent)
			if err != nil {
				t.Fatal(err)
			}
			in, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var evs []Event
			sum, err := Read(bytes.NewReader(in), p, func(e Event) { evs = append(evs, e) })
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.MarshalIndent(struct {
				Events   []Event `json:"events"`
				Dropped  int     `json:"dropped"`
				Terminal Kind    `json:"terminal"`
			}{evs, sum.Dropped, terminalKind(sum)}, "", "  ")
			got = append(got, '\n')
			golden := strings.TrimSuffix(f, ".jsonl") + ".golden"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test -run TestFixtures -update ./internal/assist/stream)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("events differ from %s:\n%s", golden, got)
			}
		})
	}
}

func terminalKind(s Summary) Kind {
	if s.Terminal == nil {
		return ""
	}
	return s.Terminal.Kind
}

func TestNewUnknownAgent(t *testing.T) {
	if _, err := New("cursor"); err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("err = %v", err)
	}
}

func readAll(t *testing.T, agent, in string) ([]Event, Summary) {
	t.Helper()
	p, err := New(agent)
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	sum, err := Read(strings.NewReader(in), p, func(e Event) { evs = append(evs, e) })
	if err != nil {
		t.Fatal(err)
	}
	return evs, sum
}

func TestRateLimitReset(t *testing.T) {
	evs, sum := readAll(t, "claude", `{"type":"result","is_error":true,"result":"usage limit reached|1793476800"}`)
	if sum.Terminal == nil || sum.Terminal.Kind != KindRateLimited || evs[0].ResetAt == nil || !evs[0].ResetAt.Equal(time.Unix(1793476800, 0)) {
		t.Fatalf("events %+v", evs)
	}
	for _, msg := range []string{"rate limit|abc", "rate limit|-5", "rate limit|99999999999999999", "rate limit"} {
		if ev := classify(msg); ev.Kind != KindRateLimited || ev.ResetAt != nil {
			t.Errorf("%q -> %+v", msg, ev)
		}
	}
	if ev := classify("  "); ev.Kind != KindError || ev.Text == "" {
		t.Errorf("empty -> %+v", ev)
	}
}

func TestClaudeEdgeCases(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"system","subtype":"hook"}`,
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":"x"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}}`,
		`{"type":"assistant","message":"x"}`,
		`{"type":"assistant","message":{"content":"plain"}}`,
		`{"type":"user","message":"x"}`,
		`{"type":"user","message":{"content":"the prompt"}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_result","tool_use_id":"unknown","content":{"odd":1}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__icaro__","input":"not an object"},{"type":"tool_use","id":"t2","name":"mcp__x__y","input":null}]}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"error":"boom"}`,
	}, "\n")
	evs, sum := readAll(t, "claude", in)
	if sum.Dropped != 3 {
		t.Errorf("dropped = %d, want 3 (two bad stream events, two bad messages minus one empty)", sum.Dropped)
	}
	var calls []Event
	for _, e := range evs {
		if e.Kind == KindToolCall {
			calls = append(calls, e)
		}
	}
	if len(calls) != 2 || calls[0].Server != "" || calls[0].Tool != "mcp__icaro__" || string(calls[0].Args) != `"not an object"` ||
		calls[1].Server != "x" || calls[1].Tool != "y" || calls[1].Args != nil {
		t.Errorf("calls = %+v", calls)
	}
	if sum.Terminal == nil || sum.Terminal.Kind != KindError || sum.Terminal.Text != "boom" {
		t.Errorf("terminal = %+v", sum.Terminal)
	}
	_, sum = readAll(t, "claude", `{"type":"result","subtype":"error_during_execution"}`)
	if sum.Terminal.Text != "error during execution" {
		t.Errorf("subtype message = %q", sum.Terminal.Text)
	}
}

func TestCodexEdgeCases(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"thread.started"}`,
		`{"type":"item.started"}`,
		`{"type":"item.completed"}`,
		`{"type":"item.started","item":{"id":"r","type":"reasoning"}}`,
		`{"type":"item.completed","item":{"id":"m","type":"agent_message","text":""}}`,
		`{"type":"item.completed","item":{"id":"c1","type":"mcp_tool_call","server":"icaro","tool":"get_run","arguments":"not json","result":{"structured":true}}}`,
		`{"type":"item.started","item":{"id":"c2","type":"mcp_tool_call","server":"icaro","tool":"get_draft"}}`,
		`{"type":"item.started","item":{"id":"c2","type":"mcp_tool_call","server":"icaro","tool":"get_draft"}}`,
		`{"type":"item.completed","item":{"id":"c2","type":"mcp_tool_call","server":"icaro","tool":"get_draft","result":null,"error":"plain error"}}`,
		`{"type":"item.completed","item":{"id":"c3","type":"mcp_tool_call","result":{"content":[{"type":"image"},{"type":"text","text":"a"},{"type":"text","text":"b"}]}}}`,
		`{"type":"turn.completed"}`,
	}, "\n")
	evs, sum := readAll(t, "codex", in)
	if sum.Dropped != 2 || sum.Terminal == nil || sum.Terminal.Kind != KindDone {
		t.Fatalf("dropped %d terminal %+v", sum.Dropped, sum.Terminal)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, string(e.Kind)+":"+e.ToolID)
	}
	want := "tool_call:c1 tool_result:c1 tool_call:c2 tool_result:c2 tool_call:c3 tool_result:c3 done:"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events %s\nwant   %s", got, want)
	}
	if string(evs[0].Args) != `{"_icaro":"arguments were not valid JSON"}` || evs[1].Result != `{"structured":true}` {
		t.Errorf("c1 = %+v %+v", evs[0], evs[1])
	}
	if !evs[3].IsError || evs[3].Result != "plain error" || evs[5].Result != "a\nb" {
		t.Errorf("results = %+v %+v", evs[3], evs[5])
	}
	_, sum = readAll(t, "codex", `{"type":"turn.failed","error":null}`)
	if sum.Terminal.Kind != KindError || sum.Terminal.Text == "" {
		t.Errorf("failed without message = %+v", sum.Terminal)
	}
}

func TestCaps(t *testing.T) {
	long := strings.Repeat("é", MaxLine) // 2 bytes per rune: over MaxLine
	in := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + long + `"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n"
	evs, sum := readAll(t, "claude", in)
	if sum.Dropped != 1 || len(evs) != 1 || evs[0].Text != "ok" {
		t.Fatalf("overlong line: dropped %d events %d", sum.Dropped, len(evs))
	}

	big := strings.Repeat("x", MaxArgs+1)
	evs, _ = readAll(t, "claude", `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"mcp__icaro__validate_workflow","input":{"yaml":"`+big+`"}}]}}`)
	if !strings.Contains(string(evs[0].Args), "arguments omitted") || !json.Valid(evs[0].Args) {
		t.Errorf("big args = %.80s", evs[0].Args)
	}
	evs, _ = readAll(t, "claude", `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"`+strings.Repeat("ü", MaxResult)+`"}]}}`)
	if len(evs[0].Result) > MaxResult+20 || !utf8.ValidString(evs[0].Result) || !strings.HasSuffix(evs[0].Result, "(truncated)") {
		t.Errorf("result len %d", len(evs[0].Result))
	}

	var b strings.Builder
	chunk := strings.Repeat("y", 100<<10)
	for range 4 {
		b.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + chunk + `"}]}}` + "\n")
	}
	evs, _ = readAll(t, "claude", b.String())
	total := 0
	for _, e := range evs {
		total += len(e.Text)
	}
	if total != MaxText || len(evs) != 3 {
		t.Errorf("text total %d over %d events, want %d", total, len(evs), MaxText)
	}

	b.Reset()
	for range MaxEvents + 5 {
		b.WriteString(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"a"}}}` + "\n")
	}
	evs, sum = readAll(t, "claude", b.String())
	if len(evs) != MaxEvents+1 || sum.Terminal == nil || sum.Terminal.Text != ErrTooManyEvents.Error() {
		t.Errorf("events %d terminal %+v", len(evs), sum.Terminal)
	}
}

func TestStopsAtTerminal(t *testing.T) {
	evs, _ := readAll(t, "codex", `{"type":"turn.completed"}`+"\n"+`{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"late"}}`)
	if len(evs) != 1 {
		t.Fatalf("events after the terminal one were emitted: %+v", evs)
	}
}

func TestReadError(t *testing.T) {
	p, _ := New("claude")
	boom := errors.New("boom")
	r := iotest.ErrReader(boom)
	if _, err := Read(r, p, func(Event) {}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if truncate("héllo", 2) != "h" || truncate("abc", 0) != "" || truncate("abc", 5) != "abc" {
		t.Error("truncate")
	}
	if args(json.RawMessage(" null ")) != nil || args(nil) != nil {
		t.Error("null args")
	}
	if got := string(args(json.RawMessage(`{ "a" : 1 }`))); got != `{"a":1}` {
		t.Errorf("compact = %s", got)
	}
	if (Event{Kind: KindText}).Terminal() || !(Event{Kind: KindAuthRequired}).Terminal() {
		t.Error("terminal")
	}
	if mcpResultText(json.RawMessage("null")) != "" || errorMessage(json.RawMessage(`{"other":1}`), "fb") != "fb" {
		t.Error("codex helpers")
	}
	if contentText(json.RawMessage(`[{"type":"text","text":"`+strings.Repeat("z", MaxResult+1)+`"},{"type":"text","text":"never"}]`)) == "" {
		t.Error("contentText")
	}
	if mcpResultText(json.RawMessage(`{"content":[{"type":"text","text":"`+strings.Repeat("z", MaxResult+1)+`"},{"type":"text","text":"never"}]}`)) == "" {
		t.Error("mcpResultText")
	}
}

// FuzzParse feeds arbitrary streams to both parsers: no panic, every
// event within the caps, and tool arguments always valid JSON.
func FuzzParse(f *testing.F) {
	files, _ := filepath.Glob("testdata/*/*/*.jsonl")
	for _, file := range files {
		if b, err := os.ReadFile(file); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		for _, agent := range []string{"claude", "codex"} {
			p, _ := New(agent)
			n := 0
			_, err := Read(bytes.NewReader(in), p, func(e Event) {
				n++
				if len(e.Args) > 0 && !json.Valid(e.Args) {
					t.Fatalf("%s: invalid args %q", agent, e.Args)
				}
				if len(e.Args) > MaxArgs || len(e.Result) > MaxResult+20 || len(e.Text) > MaxText ||
					len(e.ToolID) > maxField || len(e.Tool) > maxField || len(e.Server) > maxField || len(e.ConversationID) > maxField {
					t.Fatalf("%s: event over the caps: %+v", agent, e)
				}
			})
			if err != nil || n > MaxEvents+1 {
				t.Fatalf("%s: err %v, %d events", agent, err, n)
			}
		}
	})
}
