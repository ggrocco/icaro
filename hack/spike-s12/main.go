// Command spike-s12 runs spike S12 (ADR 0036): it summarizes a test set
// of recorded assistant chats with a llama-server, times each one and
// grades the fields against the expected values written for each chat.
// Start a server first, for example the managed runtime's container:
//
//	docker run -d --name s12 --network none --cpus 2 -v $MODELS:/models:ro -v $SOCK:/sock \
//	  ghcr.io/ggml-org/llama.cpp:server@sha256:... -m /models/Qwen3.5-2B-Q4_K_M.gguf \
//	  --host /sock/llama.sock -c 8192 -t 2 -np 1 --no-webui
//	go run ./hack/spike-s12 -socket $SOCK/llama.sock -label qwen3.5-2b
//
// Grading is automatic: each expected item is a case-insensitive regular
// expression that must match the field (or one item of a list field). A
// field expected empty is correct only if the model left it empty. Names
// the model wrote (channels, hosts, secret and workflow names) must
// appear in the chat, or they count as invented.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"icaro/internal/assist/distill"
)

type chatCase struct {
	Name     string `json:"name"`
	Workflow string `json:"workflow"`
	Note     string `json:"note"`
	Turns    []struct {
		Role string `json:"role"`
		Text string `json:"text"`
	} `json:"turns"`
	Expect map[string][][]string `json:"expect"`
}

// fields are the graded summary fields, in output order.
var fields = []string{"intent", "constraints", "preferences", "rejected", "open", "resume"}

type result struct {
	Name     string            `json:"name"`
	Tokens   int               `json:"tokens"`
	Parts    int               `json:"parts"`
	Calls    int               `json:"calls"`
	Seconds  float64           `json:"seconds"`
	Parsed   bool              `json:"parsed"`
	Error    string            `json:"error,omitempty"`
	Correct  map[string]bool   `json:"correct"`
	Lenient  map[string]bool   `json:"lenient"`
	Invented []string          `json:"invented,omitempty"`
	Summary  *distill.Summary  `json:"summary,omitempty"`
	Missed   map[string]string `json:"missed,omitempty"`
}

type report struct {
	Label        string   `json:"label"`
	Cores        int      `json:"cores"`
	PromptTokS   float64  `json:"prompt_tok_s"`
	GenTokS      float64  `json:"gen_tok_s"`
	Results      []result `json:"results"`
	Parsed       int      `json:"parsed"`
	Fields       int      `json:"fields"`
	Correct      int      `json:"correct"`
	Lenient      int      `json:"lenient"`
	InventedAny  int      `json:"chats_with_invented_names"`
	LongSeconds  float64  `json:"long_chat_seconds"`
	LongTokens   int      `json:"long_chat_tokens"`
	TotalSeconds float64  `json:"total_seconds"`
}

func main() {
	socket := flag.String("socket", "", "llama-server Unix socket")
	url := flag.String("url", "", "llama-server base URL (instead of -socket)")
	model := flag.String("model", "", "model name for an endpoint that serves several")
	label := flag.String("label", "model", "label for the report")
	cores := flag.Int("cores", 2, "cores the server was given (for the report)")
	dir := flag.String("cases", "hack/spike-s12/testdata", "directory of test chats")
	out := flag.String("out", "", "write the JSON report here")
	only := flag.String("only", "", "run only the chats whose name contains this")
	flag.Parse()
	if err := run(*socket, *url, *model, *label, *cores, *dir, *out, *only); err != nil {
		fmt.Fprintln(os.Stderr, "spike-s12:", err)
		os.Exit(1)
	}
}

func run(socket, url, model, label string, cores int, dir, out, only string) error {
	var c *distill.Client
	switch {
	case socket != "":
		c = distill.UnixClient(socket)
	case url != "":
		c = &distill.Client{BaseURL: url}
	default:
		return fmt.Errorf("give -socket or -url")
	}
	c.Model = model
	ctx := context.Background()
	if err := c.WaitReady(ctx, time.Second); err != nil {
		return err
	}
	cases, err := load(dir, only)
	if err != nil {
		return err
	}
	rep := report{Label: label, Cores: cores}
	rep.PromptTokS, rep.GenTokS = calibrate(ctx, c)
	fmt.Printf("%s: prompt %.1f tok/s, generation %.1f tok/s\n", label, rep.PromptTokS, rep.GenTokS)
	for _, cc := range cases {
		r := runCase(ctx, c, cc)
		rep.Results = append(rep.Results, r)
		rep.TotalSeconds += r.Seconds
		if r.Parsed {
			rep.Parsed++
		}
		if len(r.Invented) > 0 {
			rep.InventedAny++
		}
		for _, f := range fields {
			if _, graded := cc.Expect[f]; !graded {
				continue
			}
			rep.Fields++
			if r.Correct[f] {
				rep.Correct++
			}
			if r.Lenient[f] {
				rep.Lenient++
			}
		}
		if r.Tokens > rep.LongTokens {
			rep.LongTokens, rep.LongSeconds = r.Tokens, r.Seconds
		}
		ok := 0
		for _, v := range r.Correct {
			if v {
				ok++
			}
		}
		fmt.Printf("  %-30s %5d tok  %2d part(s)  %6.1fs  parsed=%-5v fields %d/%d  invented=%v %s\n",
			r.Name, r.Tokens, r.Parts, r.Seconds, r.Parsed, ok, len(fields), r.Invented, r.Error)
	}
	fmt.Printf("%s: %d/%d parsed, %d/%d fields correct (%.0f%%), %d/%d lenient (%.0f%%), %d chat(s) with invented names, longest chat %d tokens in %.0fs\n",
		label, rep.Parsed, len(cases), rep.Correct, rep.Fields, pct(rep.Correct, rep.Fields), rep.Lenient, rep.Fields, pct(rep.Lenient, rep.Fields),
		rep.InventedAny, rep.LongTokens, rep.LongSeconds)
	if out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		return os.WriteFile(out, b, 0o600)
	}
	return nil
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func load(dir, only string) ([]chatCase, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []chatCase
	for _, p := range paths {
		b, err := os.ReadFile(p) // #nosec G304 -- the spike reads its own test set
		if err != nil {
			return nil, err
		}
		var cc chatCase
		if err := json.Unmarshal(b, &cc); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if only == "" || strings.Contains(cc.Name, only) {
			out = append(out, cc)
		}
	}
	return out, nil
}

func runCase(ctx context.Context, c *distill.Client, cc chatCase) result {
	r := result{Name: cc.Name, Correct: map[string]bool{}, Lenient: map[string]bool{}, Missed: map[string]string{}}
	var turns []distill.Turn
	var all strings.Builder
	all.WriteString(cc.Workflow + "\n")
	for _, t := range cc.Turns {
		turns = append(turns, distill.Turn{Role: t.Role, Text: t.Text})
		all.WriteString(t.Text + "\n")
	}
	r.Tokens = tokens(ctx, c, distill.Render(turns))
	start := time.Now()
	s, u, err := distill.Summarize(ctx, c, cc.Workflow, turns)
	r.Seconds = time.Since(start).Seconds()
	r.Parts, r.Calls = u.Parts, u.Calls
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Parsed, r.Summary = true, &s
	for _, f := range fields {
		want, graded := cc.Expect[f]
		if !graded {
			continue
		}
		got := values(s, f)
		strict, lenient, missed := grade(want, got)
		r.Correct[f], r.Lenient[f] = strict, lenient
		if missed != "" {
			r.Missed[f] = missed
		}
	}
	r.Invented = invented(s, all.String())
	return r
}

// values returns a summary field as strings: one for intent and resume,
// one per item for the lists.
func values(s distill.Summary, f string) []string {
	one := func(x string) []string {
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	}
	switch f {
	case "intent":
		return one(s.Intent)
	case "resume":
		return one(s.Resume)
	case "constraints":
		return s.Constraints
	case "preferences":
		return s.Preferences
	case "open":
		return s.Open
	case "rejected":
		var out []string
		for _, r := range s.Rejected {
			out = append(out, r.What+" "+r.Why)
		}
		return out
	}
	return nil
}

// grade checks each expected item against the field: an item is a list
// of patterns that must all match the same value. Strict also requires an
// empty field when nothing was expected; lenient ignores extra items.
func grade(want [][]string, got []string) (strict, lenient bool, missed string) {
	if len(want) == 0 {
		return len(got) == 0, true, ""
	}
	joined := strings.Join(got, " | ")
	for _, item := range want {
		found := false
		for _, g := range got {
			all := true
			for _, p := range item {
				if !regexp.MustCompile("(?i)" + p).MatchString(g) {
					all = false
					break
				}
			}
			found = found || all
		}
		// Single-value fields may spread the patterns over the sentence.
		if !found && len(got) == 1 {
			found = true
			for _, p := range item {
				found = found && regexp.MustCompile("(?i)"+p).MatchString(joined)
			}
		}
		if !found {
			return false, false, strings.Join(item, " & ") + " not in: " + joined
		}
	}
	return true, true, ""
}

var namePatterns = []*regexp.Regexp{
	regexp.MustCompile(`#[a-z0-9][a-z0-9_-]+`),                                          // channels
	regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`),                                 // secret names
	regexp.MustCompile(`\b[a-z0-9-]+(?:\.[a-z0-9-]+)+\.[a-z]{2,}\b`),                    // hosts
	regexp.MustCompile(`\b[a-z][a-z0-9]*(?:-[a-z0-9]+)+\b`),                             // workflow names
	regexp.MustCompile(`\b[a-z]+\.[a-z_]+\b`),                                           // integration actions
	regexp.MustCompile(`\b(?:ghcr\.io|docker\.io)/[a-z0-9/_.-]+(?:@sha256:[0-9a-f]+)?`), // images
}

// invented lists names in the summary that the chat never mentions.
func invented(s distill.Summary, chat string) []string {
	b, _ := json.Marshal(s)
	var flat strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			flat.WriteString(x + "\n")
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	var v any
	_ = json.Unmarshal(b, &v)
	walk(v)
	low := strings.ToLower(chat)
	seen := map[string]bool{}
	var out []string
	for _, re := range namePatterns {
		for _, m := range re.FindAllString(flat.String(), -1) {
			m = strings.TrimRight(m, ".,;:")
			if !strings.Contains(low, strings.ToLower(m)) && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// tokens asks the server how many tokens text is.
func tokens(ctx context.Context, c *distill.Client, text string) int {
	body, _ := json.Marshal(map[string]any{"content": text})
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := post(ctx, c, "/tokenize", body, &out); err != nil {
		return len(text) / 4
	}
	return len(out.Tokens)
}

// calibrate measures prompt and generation speed with one plain
// completion, from the timings llama-server reports.
func calibrate(ctx context.Context, c *distill.Client) (prompt, gen float64) {
	// A fresh prefix, so the server's prompt cache cannot skip the work.
	text := time.Now().Format(time.RFC3339Nano) + "\n" + strings.Repeat("The workflow posts new bug reports to the team's channel and retries twice. ", 120)
	body, _ := json.Marshal(map[string]any{
		"model":    c.Model,
		"messages": []map[string]string{{"role": "user", "content": text + "\nSummarize in three sentences."}},
		// Thinking off, as the distiller sends it.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"max_tokens":           128, "temperature": 0, "cache_prompt": false,
	})
	var out struct {
		Timings struct {
			Prompt float64 `json:"prompt_per_second"`
			Gen    float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := post(ctx, c, "/v1/chat/completions", body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
	}
	return out.Timings.Prompt, out.Timings.Gen
}

func post(ctx context.Context, c *distill.Client, path string, body []byte, out any) error {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
