package stream

import (
	"bytes"
	"encoding/json"
	"strings"
)

// codex parses `codex exec --json`. The shapes are modeled on Codex's
// documented JSONL events; spike S11 pins them per CLI version.
type codex struct {
	// started holds the tool items already reported as calls.
	started map[string]bool
}

type codexLine struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id"`
	Item     *codexItem `json:"item"`
	Usage    *struct {
		Input  int64 `json:"input_tokens"`
		Cached int64 `json:"cached_input_tokens"`
		Output int64 `json:"output_tokens"`
	} `json:"usage"`
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

type codexItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     json.RawMessage `json:"error"`
	Status    string          `json:"status"`
	Command   string          `json:"command"`
	Output    string          `json:"aggregated_output"`
	ExitCode  *int            `json:"exit_code"`
}

func (c *codex) Line(line []byte) []Event {
	var m codexLine
	if err := json.Unmarshal(line, &m); err != nil {
		return nil
	}
	switch m.Type {
	case "thread.started":
		if m.ThreadID == "" {
			return []Event{}
		}
		return []Event{{Kind: KindSession, ConversationID: field(m.ThreadID)}}
	case "turn.started", "item.updated":
		return []Event{}
	case "item.started":
		if m.Item == nil {
			return nil
		}
		return c.call(m.Item)
	case "item.completed":
		if m.Item == nil {
			return nil
		}
		return c.completed(m.Item)
	case "turn.completed":
		var out []Event
		if m.Usage != nil {
			out = append(out, Event{Kind: KindUsage, Usage: &Usage{Input: m.Usage.Input, Output: m.Usage.Output, CacheRead: m.Usage.Cached}})
		}
		return append(out, Event{Kind: KindDone})
	case "turn.failed":
		return []Event{classify(errorMessage(m.Error, ""))}
	case "error":
		return []Event{classify(errorMessage(m.Error, m.Message))}
	}
	return nil
}

func isTool(t string) bool { return t == "mcp_tool_call" || t == "command_execution" }

func (c *codex) call(it *codexItem) []Event {
	if !isTool(it.Type) {
		return []Event{}
	}
	id := field(it.ID)
	if c.started == nil {
		c.started = map[string]bool{}
	}
	if c.started[id] {
		return []Event{}
	}
	if len(c.started) < MaxEvents {
		c.started[id] = true
	}
	ev := Event{Kind: KindToolCall, ToolID: id}
	if it.Type == "command_execution" {
		ev.Tool = "shell"
		ev.Args = args(mustMarshal(map[string]string{"command": truncate(it.Command, MaxArgs/2)}))
	} else {
		ev.Server, ev.Tool = field(it.Server), field(it.Tool)
		ev.Args = args(unquote(it.Arguments))
	}
	return []Event{ev}
}

func (c *codex) completed(it *codexItem) []Event {
	switch {
	case it.Type == "agent_message":
		if it.Text == "" {
			return []Event{}
		}
		return []Event{{Kind: KindText, Text: it.Text}}
	case isTool(it.Type):
		out := c.call(it)
		res := Event{Kind: KindToolResult, ToolID: field(it.ID)}
		if it.Type == "command_execution" {
			res.Tool = "shell"
			res.Result = result(it.Output)
			res.IsError = it.ExitCode != nil && *it.ExitCode != 0
		} else {
			res.Server, res.Tool = field(it.Server), field(it.Tool)
			if msg := errorMessage(it.Error, ""); msg != "" {
				res.Result, res.IsError = result(msg), true
			} else {
				res.Result = result(mcpResultText(it.Result))
			}
		}
		if it.Status == "failed" {
			res.IsError = true
		}
		return append(out, res)
	}
	// reasoning, todo lists, file changes: not shown in the chat.
	return []Event{}
}

// mcpResultText flattens an MCP CallToolResult's text content, falling
// back to its JSON.
func mcpResultText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &r) == nil && len(r.Content) > 0 {
		var b strings.Builder
		for _, p := range r.Content {
			if p.Type == "text" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
				if b.Len() > MaxResult {
					break
				}
			}
		}
		return b.String()
	}
	return string(raw)
}

// errorMessage reads an error that is a string, an object with a
// message, or absent.
func errorMessage(raw json.RawMessage, fallback string) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && string(raw) != "null" {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var o struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &o) == nil && o.Message != "" {
			return o.Message
		}
	}
	return fallback
}

// unquote accepts arguments sent as a JSON object or as a JSON string
// holding one.
func unquote(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return json.RawMessage(s)
	}
	return raw
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
