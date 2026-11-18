package distill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Summary is what the model extracts from a chat (ADR 0036). Every field
// may be empty: the model is told to leave out what the chat does not say.
type Summary struct {
	Intent      string     `json:"intent"`
	Constraints []string   `json:"constraints"`
	Preferences []string   `json:"preferences"`
	Rejected    []Rejected `json:"rejected"`
	Open        []string   `json:"open"`
	Resume      string     `json:"resume"`
}

// Rejected is a proposal the person turned down.
type Rejected struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// Caps on the model's output.
const (
	maxSentence = 400
	maxResume   = 800
	maxItems    = 8
)

// SummarySchema is the JSON schema the runtime constrains output to.
var SummarySchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["intent", "constraints", "preferences", "rejected", "open", "resume"],
  "properties": {
    "intent": {"type": "string", "maxLength": 400},
    "constraints": {"type": "array", "maxItems": 8, "items": {"type": "string", "maxLength": 400}},
    "preferences": {"type": "array", "maxItems": 8, "items": {"type": "string", "maxLength": 400}},
    "rejected": {"type": "array", "maxItems": 8, "items": {
      "type": "object", "additionalProperties": false, "required": ["what", "why"],
      "properties": {"what": {"type": "string", "maxLength": 400}, "why": {"type": "string", "maxLength": 400}}
    }},
    "open": {"type": "array", "maxItems": 8, "items": {"type": "string", "maxLength": 400}},
    "resume": {"type": "string", "maxLength": 800}
  }
}`)

// systemPrompt tells the model what to extract. It only reads the
// person's and the assistant's words, never run logs (ADR 0036).
const systemPrompt = `You summarize a chat between a person and Ícaro's workflow assistant, which helps write automation workflows. Answer with JSON only.

Use only what the chat says. Never invent the names of workflows, integrations, secrets, hosts, channels or people. Leave a field empty ("" or []) when the chat does not say.

- intent: what the workflow is for, in one or two sentences.
- constraints: requirements the person stated (when it runs, what it must or must not do).
- preferences: the person's taste that is not a requirement (style, tone, a tool or profile they favour). Do not restate the workflow's configuration here.
- rejected: everything the person said no to, replaced or asked to remove: a proposal, a value they changed (an earlier time, a profile they swapped), an integration they gave up. Give the reason they gave, or "" if none.
- open: questions the chat itself left unanswered, or work the person said they will do later. Not questions of your own.
- resume: where the chat left off, in at most three sentences: what was done last (validated, saved and its version, or not saved yet) and the next step if one was named.`

// mergePrompt combines summaries of consecutive parts of one chat.
const mergePrompt = systemPrompt + `

You are given JSON summaries of consecutive parts of one chat, oldest first. Merge them into one summary of the whole chat: later parts win where they disagree, drop duplicates, and keep the open questions that later parts did not answer.`

// Turn is one message of a chat.
type Turn struct {
	Role string // user | assistant
	Text string
}

// ErrInvalidSummary reports model output that is not a summary.
var ErrInvalidSummary = errors.New("the model's answer is not a valid summary")

// ParseSummary reads model output (untrusted) into a Summary, trimming
// and capping every field and dropping empty items.
func ParseSummary(b []byte) (Summary, error) {
	var s Summary
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Summary{}, fmt.Errorf("%w: %v", ErrInvalidSummary, err)
	}
	s.Intent = text(s.Intent, maxSentence)
	s.Resume = text(s.Resume, maxResume)
	s.Constraints, s.Preferences, s.Open = list(s.Constraints), list(s.Preferences), list(s.Open)
	var rej []Rejected
	for _, r := range s.Rejected {
		r.What, r.Why = text(r.What, maxSentence), text(r.Why, maxSentence)
		if r.What != "" && len(rej) < maxItems {
			rej = append(rej, r)
		}
	}
	s.Rejected = rej
	return s, nil
}

// Empty reports a summary with nothing in it.
func (s Summary) Empty() bool {
	return s.Intent == "" && s.Resume == "" && len(s.Constraints)+len(s.Preferences)+len(s.Rejected)+len(s.Open) == 0
}

// placeholders are what small models write for "nothing".
var placeholders = map[string]bool{"none": true, "n/a": true, "na": true, "null": true, "nothing": true, "-": true, "not specified": true, "not mentioned": true}

func text(s string, n int) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	if placeholders[strings.ToLower(strings.TrimRight(s, ".!"))] {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func list(xs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		x = text(x, maxSentence)
		if x != "" && !seen[strings.ToLower(x)] && len(out) < maxItems {
			seen[strings.ToLower(x)] = true
			out = append(out, x)
		}
	}
	return out
}

// Render is a chat as the model reads it.
func Render(turns []Turn) string {
	var b strings.Builder
	for _, t := range turns {
		who := "PERSON"
		if t.Role == "assistant" {
			who = "ASSISTANT"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", who, strings.TrimSpace(t.Text))
	}
	return b.String()
}

// Chunk splits turns into parts of at most limit characters, never
// splitting a turn unless one turn alone is over the limit.
func Chunk(turns []Turn, limit int) [][]Turn {
	var out [][]Turn
	var cur []Turn
	size := 0
	for _, t := range turns {
		for len(t.Text) > limit {
			cut := limit
			for cut > 0 && !utf8.RuneStart(t.Text[cut]) {
				cut--
			}
			if len(cur) > 0 {
				out, cur, size = append(out, cur), nil, 0
			}
			out = append(out, []Turn{{Role: t.Role, Text: t.Text[:cut]}})
			t.Text = t.Text[cut:]
		}
		if size+len(t.Text) > limit && len(cur) > 0 {
			out, cur, size = append(out, cur), nil, 0
		}
		cur = append(cur, t)
		size += len(t.Text)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// Limits of one summary call.
const (
	// ChunkChars keeps one part, the prompt and the answer inside the
	// runtime's 12k-token context even at 3 characters per token (chats
	// average about 4). Spike S12: one call on a 6,000-token chat is
	// faster and better than two parts and a merge.
	ChunkChars = 32000
	maxTokens  = 700
)

// Usage counts what a summary cost.
type Usage struct {
	Calls int `json:"calls"`
	Parts int `json:"parts"`
}

// Summarize asks c for a summary of a chat about workflow: one call for a
// short chat; for a long one, a summary per part and then a merge.
func Summarize(ctx context.Context, c Completer, workflow string, turns []Turn) (Summary, Usage, error) {
	var u Usage
	header := ""
	if workflow != "" {
		header = "The chat is about the workflow " + workflow + ".\n\n"
	}
	parts := Chunk(turns, ChunkChars)
	u.Parts = len(parts)
	if len(parts) == 0 {
		return Summary{}, u, nil
	}
	var sums []Summary
	for _, p := range parts {
		u.Calls++
		out, err := c.Complete(ctx, systemPrompt, header+Render(p), SummarySchema, maxTokens)
		if err != nil {
			return Summary{}, u, err
		}
		s, err := ParseSummary([]byte(out))
		if err != nil {
			return Summary{}, u, err
		}
		sums = append(sums, s)
	}
	if len(sums) == 1 {
		return sums[0], u, nil
	}
	in, _ := json.MarshalIndent(sums, "", "  ")
	u.Calls++
	out, err := c.Complete(ctx, mergePrompt, header+string(in), SummarySchema, maxTokens)
	if err != nil {
		return Summary{}, u, err
	}
	s, err := ParseSummary([]byte(out))
	return s, u, err
}
