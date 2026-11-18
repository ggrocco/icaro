// Package stream turns an agent CLI's headless event stream into one
// normalized stream of chat events (ADR 0035). The input comes out of a
// container that runs model output, so it is untrusted: lines, fields and
// event counts are capped, and anything a parser does not recognize is
// dropped instead of failing the turn.
package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind is the type of a normalized event.
type Kind string

// Event kinds.
const (
	// KindSession carries the CLI's conversation id, used to resume.
	KindSession Kind = "session"
	// KindText is assistant text (a delta or a whole block).
	KindText Kind = "text"
	// KindToolCall is a tool the assistant called.
	KindToolCall Kind = "tool_call"
	// KindToolResult is the (truncated) result of a tool call.
	KindToolResult Kind = "tool_result"
	// KindUsage reports tokens (and the CLI's cost figure, if any).
	KindUsage Kind = "usage"
	// KindRateLimited ends a turn on a provider usage limit.
	KindRateLimited Kind = "rate_limited"
	// KindAuthRequired ends a turn because the profile is not signed in.
	KindAuthRequired Kind = "auth_required"
	// KindError reports a CLI or stream error.
	KindError Kind = "error"
	// KindDone ends a turn that completed.
	KindDone Kind = "done"
)

// Caps on untrusted input.
const (
	// MaxLine is the longest stream line read; longer lines are dropped.
	MaxLine = 1 << 20
	// MaxEvents is the most events one turn may produce.
	MaxEvents = 10000
	// MaxArgs caps a tool call's arguments (larger ones are replaced by a
	// note, since truncated JSON would not parse).
	MaxArgs = 16 << 10
	// MaxResult caps a tool result's text.
	MaxResult = 4 << 10
	// MaxText caps the assistant text of one turn.
	MaxText = 256 << 10
	// maxField caps every other string field (ids, names, messages).
	maxField = 512
)

// Usage is a turn's token count, from the CLI's own usage fields.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
	// CostUSD is the CLI's own figure; it is only meaningful for API-key
	// (inject) profiles and is an estimate even there.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Event is one normalized chat event.
type Event struct {
	Kind Kind `json:"kind"`
	// Text is assistant text, or an error or limit message.
	Text string `json:"text,omitempty"`
	// ConversationID is the CLI's id for resuming (KindSession).
	ConversationID string `json:"conversation_id,omitempty"`
	// ToolID pairs a call with its result.
	ToolID string `json:"tool_id,omitempty"`
	// Server and Tool name the MCP server and tool.
	Server string `json:"server,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// Args are the call's arguments (valid JSON, capped).
	Args json.RawMessage `json:"args,omitempty"`
	// Result is the tool result's text (capped).
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Usage   *Usage `json:"usage,omitempty"`
	// ResetAt is when a rate limit ends, when the CLI says.
	ResetAt *time.Time `json:"reset_at,omitempty"`
}

// Terminal reports whether the event ends a turn.
func (e Event) Terminal() bool {
	switch e.Kind {
	case KindDone, KindError, KindRateLimited, KindAuthRequired:
		return true
	}
	return false
}

// Parser turns one stream line into events. Parsers keep per-turn state;
// use a new one for every turn.
type Parser interface {
	Line(line []byte) []Event
}

// New returns the parser for an agent (the profile's `agent` field).
func New(agent string) (Parser, error) {
	switch agent {
	case "claude":
		return &claude{}, nil
	case "codex":
		return &codex{}, nil
	}
	return nil, fmt.Errorf("no headless chat stream for agent %q (known: claude, codex)", agent)
}

// Summary describes a whole turn's stream.
type Summary struct {
	Events  int
	Dropped int // overlong or unparseable lines
	// Terminal is the event that ended the turn, nil if the stream ended
	// without one (the CLI died or was cancelled).
	Terminal *Event
}

// ErrTooManyEvents reports a stream that exceeded MaxEvents.
var ErrTooManyEvents = errors.New("the agent's event stream exceeded its limit")

// Read parses r line by line and calls emit for every event, applying the
// caps. It stops after the first terminal event, at MaxEvents, or at EOF.
// Errors from r are returned; a malformed stream is not an error.
func Read(r io.Reader, p Parser, emit func(Event)) (Summary, error) {
	var sum Summary
	br := bufio.NewReaderSize(r, 64<<10)
	text := 0
	for {
		line, tooLong, err := readLine(br)
		if tooLong {
			sum.Dropped++
		} else if len(bytes.TrimSpace(line)) > 0 {
			evs := p.Line(line)
			if evs == nil {
				sum.Dropped++
			}
			for _, ev := range evs {
				if ev.Kind == KindText {
					if text >= MaxText {
						continue
					}
					ev.Text = truncate(ev.Text, MaxText-text)
					text += len(ev.Text)
				}
				if sum.Events >= MaxEvents {
					e := Event{Kind: KindError, Text: ErrTooManyEvents.Error()}
					emit(e)
					sum.Terminal = &e
					return sum, nil
				}
				sum.Events++
				emit(ev)
				if ev.Terminal() {
					sum.Terminal = &ev
					return sum, nil
				}
			}
		}
		if err == io.EOF {
			return sum, nil
		}
		if err != nil {
			return sum, err
		}
	}
}

// readLine reads one line, discarding the rest of a line over MaxLine.
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, isPrefix, rerr := br.ReadLine()
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > MaxLine {
				line, tooLong = nil, true
			}
		}
		if rerr != nil {
			return line, tooLong, rerr
		}
		if !isPrefix {
			return line, tooLong, nil
		}
	}
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// field caps a short string field.
func field(s string) string { return truncate(s, maxField) }

// args caps tool arguments; they must stay valid JSON.
func args(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if !json.Valid(raw) {
		return json.RawMessage(`{"_icaro":"arguments were not valid JSON"}`)
	}
	if len(raw) > MaxArgs {
		return json.RawMessage(`{"_icaro":"arguments omitted: ` + strconv.Itoa(len(raw)) + ` bytes"}`)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil
	}
	return buf.Bytes()
}

// result caps a tool result's text.
func result(s string) string {
	if len(s) <= MaxResult {
		return s
	}
	return truncate(s, MaxResult) + "\n… (truncated)"
}

// classify turns an error message into the terminal event it stands for.
// The patterns are modeled on the CLIs' documented messages; spike S11
// replaces them with recorded ones.
func classify(msg string) Event {
	msg = field(strings.TrimSpace(msg))
	low := strings.ToLower(msg)
	switch {
	case containsAny(low, "/login", "not logged in", "invalid api key", "oauth token", "authentication_error", "please log in", "sign in again", "unauthorized", "401"):
		return Event{Kind: KindAuthRequired, Text: msg}
	case containsAny(low, "usage limit", "limit reached", "rate limit", "rate_limit", "429", "too many requests"):
		ev := Event{Kind: KindRateLimited, Text: msg}
		if ev.ResetAt = resetTime(msg); ev.ResetAt != nil {
			ev.Text = strings.TrimSpace(msg[:strings.LastIndexByte(msg, '|')])
		}
		return ev
	}
	if msg == "" {
		msg = "the agent reported an error"
	}
	return Event{Kind: KindError, Text: msg}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// resetTime reads a "…|<unix seconds>" suffix, the form Claude Code has
// used for usage-limit messages.
func resetTime(msg string) *time.Time {
	i := strings.LastIndexByte(msg, '|')
	if i < 0 {
		return nil
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(msg[i+1:]), 10, 64)
	if err != nil || sec <= 0 || sec > 1<<40 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}
