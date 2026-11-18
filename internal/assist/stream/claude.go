package stream

import (
	"encoding/json"
	"strings"
)

// claude parses `claude -p --output-format stream-json --verbose`
// (optionally with --include-partial-messages). The shapes are modeled on
// Claude Code's documented stream; spike S11 pins them per CLI version.
type claude struct {
	// partial is set once text deltas arrive; whole text blocks of the
	// assistant messages that follow would repeat them.
	partial bool
	// tools maps a tool_use id to its tool, for results.
	tools map[string][2]string
}

type claudeLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	CostUSD   float64         `json:"total_cost_usd"`
	Usage     *claudeUsage    `json:"usage"`
	Error     string          `json:"error"`
}

type claudeUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (c *claude) Line(line []byte) []Event {
	var m claudeLine
	if err := json.Unmarshal(line, &m); err != nil {
		return nil
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" && m.SessionID != "" {
			return []Event{{Kind: KindSession, ConversationID: field(m.SessionID)}}
		}
		return []Event{}
	case "stream_event":
		return c.delta(m.Event)
	case "assistant":
		return c.assistant(m.Message)
	case "user":
		return c.user(m.Message)
	case "result":
		return c.result(m)
	}
	return nil
}

func (c *claude) delta(raw json.RawMessage) []Event {
	var ev struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return nil
	}
	if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
		c.partial = true
		return []Event{{Kind: KindText, Text: ev.Delta.Text}}
	}
	return []Event{}
}

func blocks(raw json.RawMessage) ([]claudeBlock, bool) {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return nil, false
	}
	var bs []claudeBlock
	if json.Unmarshal(msg.Content, &bs) != nil {
		// A plain-string content (an echoed prompt) carries nothing new.
		return nil, true
	}
	return bs, true
}

func (c *claude) assistant(raw json.RawMessage) []Event {
	bs, ok := blocks(raw)
	if !ok {
		return nil
	}
	out := []Event{}
	for _, b := range bs {
		switch b.Type {
		case "text":
			if !c.partial && b.Text != "" {
				out = append(out, Event{Kind: KindText, Text: b.Text})
			}
		case "tool_use":
			server, tool := splitTool(b.Name)
			if c.tools == nil {
				c.tools = map[string][2]string{}
			}
			id := field(b.ID)
			if len(c.tools) < MaxEvents {
				c.tools[id] = [2]string{server, tool}
			}
			out = append(out, Event{Kind: KindToolCall, ToolID: id, Server: server, Tool: tool, Args: args(b.Input)})
		}
	}
	return out
}

func (c *claude) user(raw json.RawMessage) []Event {
	bs, ok := blocks(raw)
	if !ok {
		return nil
	}
	out := []Event{}
	for _, b := range bs {
		if b.Type != "tool_result" {
			continue
		}
		id := field(b.ToolUseID)
		names := c.tools[id]
		out = append(out, Event{Kind: KindToolResult, ToolID: id, Server: names[0], Tool: names[1], Result: result(contentText(b.Content)), IsError: b.IsError})
	}
	return out
}

func (c *claude) result(m claudeLine) []Event {
	var out []Event
	if m.Usage != nil {
		out = append(out, Event{Kind: KindUsage, Usage: &Usage{
			Input: m.Usage.Input, Output: m.Usage.Output, CacheRead: m.Usage.CacheRead, CacheWrite: m.Usage.CacheWrite, CostUSD: m.CostUSD,
		}})
	}
	if m.IsError || strings.HasPrefix(m.Subtype, "error") {
		msg := m.Result
		if msg == "" {
			msg = m.Error
		}
		if msg == "" {
			msg = strings.ReplaceAll(m.Subtype, "_", " ")
		}
		return append(out, classify(msg))
	}
	return append(out, Event{Kind: KindDone})
}

// contentText flattens a tool_result's content: a string, or a list of
// text blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
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

// splitTool splits Claude Code's "mcp__<server>__<tool>" names; built-in
// tools have no server.
func splitTool(name string) (server, tool string) {
	name = field(name)
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		if s, t, ok := strings.Cut(rest, "__"); ok && s != "" && t != "" {
			return s, t
		}
	}
	return "", name
}
