package assist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"icaro/internal/assist/stream"
	"icaro/internal/memory"
	"icaro/internal/service"
	"icaro/internal/store"
)

// Memory sources and types of what the distiller writes (ADR 0036).
const (
	SourceLesson  = "assist:lesson"
	SourceFact    = "assist:integration"
	SourceRun     = "assist:run"
	SourceSaved   = "assist:saved"
	SourceExample = "assist:example"
	SourceSummary = "assist:summary"

	ConfidenceExact = "exact"
	ConfidenceModel = "model"
)

// Origin is the memory origin of a chat's distilled memories.
func Origin(chatID string) string { return "distill:" + chatID }

// GlobalProject scopes the memories of chats that have no project.
const GlobalProject = "assistant"

// MemoryScope is where a chat's memories live.
func MemoryScope(c *store.Chat) memory.Scope {
	p := c.ProjectID
	if p == "" {
		p = GlobalProject
	}
	return memory.Scope{ProjectID: p, Origin: "distill"}
}

// issue is a validator issue as a tool result reports it.
type issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// toolCheck is one validate_workflow or propose_workflow call and result.
type toolCheck struct {
	yaml   string
	valid  bool
	issues []issue
	known  bool // the result could be read
}

// facts is what stage 1 reads from a chat.
type facts struct {
	checks       []toolCheck
	turns        int
	usedMemories int
	saves        []saveNote
	proposals    int
}

type saveNote struct {
	turn    int // assistant turns completed before it
	seconds int // since the chat was created
}

var (
	reValid   = regexp.MustCompile(`"valid"\s*:\s*(true|false)`)
	reIssue   = regexp.MustCompile(`"path"\s*:\s*"((?:[^"\\]|\\.)*)"\s*,\s*"message"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	reSaved   = regexp.MustCompile(`^Saved as (\S+) v(\d+)\.$`)
	reSecrets = regexp.MustCompile(`secrets\.([A-Za-z][A-Za-z0-9_.-]{0,127})`)
)

// readFacts walks a chat's log.
func readFacts(c *store.Chat, entries []entry) facts {
	var f facts
	pending := map[string]string{} // tool id -> yaml of a check in flight
	checkTool := func(tool string) bool { return tool == "validate_workflow" || tool == "propose_workflow" }
	for _, e := range entries {
		switch e.Type {
		case EntryMemory:
			f.usedMemories += len(e.Memories)
		case EntryStatus:
			if e.MessageID != "" && e.Status == store.TurnDone {
				f.turns++
			}
			if m := reSaved.FindStringSubmatch(e.Note); m != nil {
				f.saves = append(f.saves, saveNote{turn: f.turns, seconds: int(e.Time.Sub(c.CreatedAt).Seconds())})
			}
		case EntryEvent:
			ev := e.Event
			if ev == nil || !checkTool(ev.Tool) || ev.Server != "icaro" && ev.Server != "" {
				continue
			}
			switch ev.Kind {
			case stream.KindToolCall:
				var args struct {
					YAML string `json:"yaml"`
				}
				_ = json.Unmarshal(ev.Args, &args)
				pending[ev.ToolID] = args.YAML
				if ev.Tool == "propose_workflow" {
					f.proposals++
				}
			case stream.KindToolResult:
				y, ok := pending[ev.ToolID]
				if !ok {
					continue
				}
				delete(pending, ev.ToolID)
				f.checks = append(f.checks, parseCheck(y, ev.Result, ev.IsError))
			}
		}
	}
	return f
}

// parseCheck reads a validate/propose result, which may be cut short.
func parseCheck(y, result string, isError bool) toolCheck {
	ch := toolCheck{yaml: y}
	var full struct {
		Valid  *bool   `json:"valid"`
		Issues []issue `json:"issues"`
	}
	if json.Unmarshal([]byte(result), &full) == nil && full.Valid != nil {
		ch.valid, ch.issues, ch.known = *full.Valid, full.Issues, true
		return ch
	}
	if m := reValid.FindStringSubmatch(result); m != nil {
		ch.valid, ch.known = m[1] == "true", true
		for _, im := range reIssue.FindAllStringSubmatch(result, 20) {
			ch.issues = append(ch.issues, issue{Path: unescape(im[1]), Message: unescape(im[2])})
		}
	}
	if isError && !ch.known {
		ch.known = false
	}
	return ch
}

func unescape(s string) string {
	var out string
	if json.Unmarshal([]byte(`"`+s+`"`), &out) == nil {
		return out
	}
	return s
}

// stats turns facts into the chat's metrics.
func (f facts) stats(chatID string) *store.ChatStats {
	st := &store.ChatStats{ChatID: chatID, UsedMemories: f.usedMemories, Turns: f.turns, Validations: -1, Proposals: f.proposals,
		Saves: len(f.saves), TurnsToSave: -1, SecondsToSave: -1}
	n := 0
	for _, ch := range f.checks {
		if !ch.known {
			continue
		}
		n++
		if ch.valid {
			st.Validations = n
			break
		}
	}
	if len(f.saves) > 0 {
		st.TurnsToSave, st.SecondsToSave = f.saves[0].turn, max(f.saves[0].seconds, 0)
	}
	return st
}

// lessons pairs each failing check with the next passing one: the
// issues it had and the change that cleared them.
func (f facts) lessons(workflowName string) []memory.ImportItem {
	var out []memory.ImportItem
	var failing *toolCheck
	seen := map[string]bool{}
	for i := range f.checks {
		ch := f.checks[i]
		if !ch.known {
			continue
		}
		if !ch.valid {
			failing = &f.checks[i]
			continue
		}
		if failing == nil {
			continue
		}
		diff := ""
		if failing.yaml != "" && ch.yaml != "" {
			diff = changedLines(failing.yaml, ch.yaml, 24)
		}
		for _, is := range failing.issues {
			key := "lesson:" + hash(is.Path+"\x00"+is.Message)
			if seen[key] {
				continue
			}
			seen[key] = true
			body := fmt.Sprintf("The validator reported %q at `%s`.", is.Message, is.Path)
			if is.Hint != "" {
				body += " Hint: " + is.Hint + "."
			}
			if diff != "" {
				body += "\n\nThe change that cleared it:\n\n```diff\n" + diff + "\n```"
			}
			out = append(out, memory.ImportItem{
				Key: key, Kind: memory.KindFinding, Title: clip("Validator: "+is.Message+" at "+is.Path, 200), Body: body, Source: SourceLesson,
				Metadata: map[string]any{"type": "validator_lesson", "path": is.Path, "message": is.Message, "workflow": workflowName, "confidence": ConfidenceExact},
			})
		}
		failing = nil
	}
	return out
}

// integrationFacts reads a valid draft: the actions it uses with their
// inputs, the secrets it references and the events that trigger it.
func integrationFacts(workflowName, y string) []memory.ImportItem {
	var doc struct {
		Triggers []struct {
			Integration string `yaml:"integration"`
			Event       string `yaml:"event"`
		} `yaml:"triggers"`
		Steps []struct {
			ID    string         `yaml:"id"`
			Uses  string         `yaml:"uses"`
			Input map[string]any `yaml:"input"`
		} `yaml:"steps"`
	}
	if yaml.Unmarshal([]byte(y), &doc) != nil {
		return nil
	}
	secrets := map[string]bool{}
	for _, m := range reSecrets.FindAllStringSubmatch(y, -1) {
		secrets[m[1]] = true
	}
	var out []memory.ImportItem
	for _, st := range doc.Steps {
		if st.Uses == "" {
			continue
		}
		inputs := make([]string, 0, len(st.Input))
		for k := range st.Input {
			inputs = append(inputs, k)
		}
		sort.Strings(inputs)
		body := fmt.Sprintf("Workflow %s uses the action `%s` (step %s) with inputs: %s.", workflowName, st.Uses, st.ID, orNone(inputs))
		if s := keys(secrets); len(s) > 0 {
			body += " The workflow references the secrets " + strings.Join(s, ", ") + "."
		}
		out = append(out, memory.ImportItem{
			Key: "uses:" + st.Uses, Kind: memory.KindFinding, Title: clip(fmt.Sprintf("Integration action %s, as used in %s", st.Uses, workflowName), 200),
			Body: body, Source: SourceFact,
			Metadata: map[string]any{"type": "integration_fact", "action": st.Uses, "workflow": workflowName, "confidence": ConfidenceExact},
		})
	}
	for _, tr := range doc.Triggers {
		if tr.Integration == "" {
			continue
		}
		ev := tr.Integration + "." + tr.Event
		out = append(out, memory.ImportItem{
			Key: "trigger:" + ev, Kind: memory.KindFinding, Title: clip(fmt.Sprintf("Trigger %s, as used in %s", ev, workflowName), 200),
			Body: fmt.Sprintf("Workflow %s is triggered by the %s event %s.", workflowName, tr.Integration, tr.Event), Source: SourceFact,
			Metadata: map[string]any{"type": "integration_fact", "trigger": ev, "workflow": workflowName, "confidence": ConfidenceExact},
		})
	}
	return out
}

// RunsReader is the part of the service layer stage 1 reads runs and
// versions from.
type RunsReader interface {
	GetRun(ctx context.Context, id string) (*service.RunInfo, error)
	ListRuns(ctx context.Context, workflowName, status string, limit int) ([]*service.RunInfo, error)
	ListWorkflowVersions(ctx context.Context, name string) ([]service.VersionInfo, error)
	GetWorkflowVersion(ctx context.Context, name string, version int) (*service.VersionInfo, error)
}

// outcomes reads the runs a person approved in the chat, the versions
// saved through it, and saved versions that later ran green.
func outcomes(ctx context.Context, r RunsReader, c *store.Chat, approvals []*store.ChatApproval, rx *redactor) []memory.ImportItem {
	var out []memory.ImportItem
	for _, a := range approvals {
		if a.Result == "" {
			continue
		}
		run, err := r.GetRun(ctx, a.Result)
		if err != nil || !service.Terminal(run.Status) {
			continue
		}
		title := fmt.Sprintf("Run of %s v%d %s", run.Workflow, run.Version, run.Status)
		body := fmt.Sprintf("A run of %s v%d that a person approved in an assistant chat %s.", run.Workflow, run.Version, run.Status)
		for _, st := range run.Steps {
			if st.Status == "failed" {
				title += ", step " + st.ID + " failed"
				body += fmt.Sprintf(" Step %s failed: %s", st.ID, rx.RedactString(clip(st.Error, 300)))
				break
			}
		}
		out = append(out, memory.ImportItem{
			Key: "run:" + run.ID, Kind: memory.KindFinding, Title: clip(title, 200), Body: body, Source: SourceRun,
			Metadata: map[string]any{"type": "run_outcome", "run": run.ID, "status": run.Status, "workflow": run.Workflow, "confidence": ConfidenceExact},
		})
	}
	if c.Workflow == "" {
		return out
	}
	versions, err := r.ListWorkflowVersions(ctx, c.Workflow)
	if err != nil {
		return out
	}
	succeeded, _ := r.ListRuns(ctx, c.Workflow, "succeeded", 200)
	for _, v := range versions {
		if !strings.HasSuffix(v.Actor, " via assistant "+c.ID) {
			continue
		}
		out = append(out, memory.ImportItem{
			Key: fmt.Sprintf("saved:%s:%d", c.Workflow, v.Version), Kind: memory.KindDecision,
			Title:  fmt.Sprintf("Saved %s v%d from an assistant chat", c.Workflow, v.Version),
			Body:   fmt.Sprintf("%s saved %s v%d after accepting the assistant's proposal.", strings.TrimSuffix(v.Actor, " via assistant "+c.ID), c.Workflow, v.Version),
			Source: SourceSaved, Metadata: map[string]any{"type": "saved", "workflow": c.Workflow, "version": v.Version, "confidence": ConfidenceExact},
		})
		for _, run := range succeeded {
			if run.Version != v.Version {
				continue
			}
			full, err := r.GetWorkflowVersion(ctx, c.Workflow, v.Version)
			if err != nil {
				break
			}
			out = append(out, memory.ImportItem{
				Key: fmt.Sprintf("example:%s:%d", c.Workflow, v.Version), Kind: memory.KindFinding,
				Title:  fmt.Sprintf("Working example: %s v%d ran green", c.Workflow, v.Version),
				Body:   "This version, written with the assistant, ran successfully:\n\n```yaml\n" + clipBytes(rx.RedactString(full.YAML), 4<<10) + "\n```",
				Source: SourceExample, Metadata: map[string]any{"type": "working_example", "workflow": c.Workflow, "version": v.Version, "confidence": ConfidenceExact},
			})
			break
		}
	}
	return out
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "\n# … (cut)"
}

// changedLines is a line diff of a and b with only the changed lines
// ("- " removed, "+ " added), at most limit of them.
func changedLines(a, b string, limit int) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	if len(x)*len(y) > 1<<20 {
		return "" // too big to diff here
	}
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for (i < n || j < m) && len(out) < limit {
		switch {
		case i < n && j < m && x[i] == y[j]:
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, "+ "+y[j])
			j++
		default:
			out = append(out, "- "+x[i])
			i++
		}
	}
	return strings.Join(out, "\n")
}
