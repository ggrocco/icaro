// Package mcp is a small, dependency-free Model Context Protocol server:
// JSON-RPC 2.0 over stdio (newline-delimited) and streamable HTTP, with a
// registry of tools and resources. Other packages register tools; every
// call carries the Caller the transport authenticated, so tools enforce
// scope server-side (ADR 0004, architecture §10).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// Protocol versions this server speaks, newest first.
var SupportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// LatestVersion is the version offered when a client asks for an unknown one.
var LatestVersion = SupportedVersions[0]

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Caller is the authenticated identity of a request. Transports set it;
// tool arguments can never change it.
type Caller struct {
	// ProjectID scopes project data (memory). Empty means no project scope.
	ProjectID string
	Branch    string
	Worktree  string
	Agent     string
	SessionID string
	// Principal names who is calling (token name, "local", "session:<id>").
	Principal string
	// Scope is the API token scope: read | write | admin.
	Scope string
}

type callerKey struct{}

// WithCaller attaches c to ctx.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller on ctx.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Content is one item of a tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// ToolResult is the result of tools/call.
type ToolResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// JSONResult returns v as structured content plus its JSON text, which
// clients that ignore structured content still read.
func JSONResult(v any) *ToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ErrorResult("encode result: " + err.Error())
	}
	return &ToolResult{Content: []Content{{Type: "text", Text: string(b)}}, StructuredContent: v}
}

// TextResult returns plain text.
func TextResult(s string) *ToolResult {
	return &ToolResult{Content: []Content{{Type: "text", Text: s}}}
}

// ErrorResult reports a tool-level failure the model should see and
// correct (not a protocol error).
func ErrorResult(msg string) *ToolResult {
	return &ToolResult{Content: []Content{{Type: "text", Text: msg}}, IsError: true}
}

// Annotations are tool hints for clients.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Bool returns a pointer for annotation hints.
func Bool(b bool) *bool { return &b }

// Handler runs a tool. Returning an error yields an isError result with
// the error text; protocol errors are reserved for malformed calls.
type Handler func(ctx context.Context, args json.RawMessage) (*ToolResult, error)

// Tool is a registered tool.
type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  *Annotations    `json:"annotations,omitempty"`
	Handler      Handler         `json:"-"`
}

// Resource is a registered resource.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	// Read returns the resource text.
	Read func(ctx context.Context) (string, error) `json:"-"`
}

// Server dispatches MCP requests. Register tools and resources before
// serving; the registry is read-only while requests run.
type Server struct {
	name         string
	version      string
	instructions string
	tools        map[string]Tool
	resources    map[string]Resource
}

// NewServer creates a server that reports name/version in initialize.
func NewServer(name, version, instructions string) *Server {
	return &Server{name: name, version: version, instructions: instructions, tools: map[string]Tool{}, resources: map[string]Resource{}}
}

// AddTool registers a tool; a duplicate name is a programming error.
func (s *Server) AddTool(t Tool) {
	if t.Name == "" || t.Handler == nil {
		panic("mcp: tool needs a name and a handler")
	}
	if _, dup := s.tools[t.Name]; dup {
		panic("mcp: duplicate tool " + t.Name)
	}
	if len(t.InputSchema) == 0 {
		t.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	s.tools[t.Name] = t
}

// AddResource registers a resource.
func (s *Server) AddResource(r Resource) {
	if r.URI == "" || r.Read == nil {
		panic("mcp: resource needs a uri and a reader")
	}
	if _, dup := s.resources[r.URI]; dup {
		panic("mcp: duplicate resource " + r.URI)
	}
	s.resources[r.URI] = r
}

// ToolNames lists registered tools, sorted.
func (s *Server) ToolNames() []string {
	out := make([]string, 0, len(s.tools))
	for n := range s.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Request is a JSON-RPC request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports a message without an id.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

func errResp(id json.RawMessage, code int, msg string) *Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}

// HandleMessage processes one raw message (a request, a notification,
// a response from the client, or a batch). It returns the encoded reply,
// or nil when nothing should be sent back.
func (s *Server) HandleMessage(ctx context.Context, raw []byte) []byte {
	trimmed := trimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return mustJSON(errResp(nil, CodeParseError, "parse error: "+err.Error()))
		}
		if len(batch) == 0 {
			return mustJSON(errResp(nil, CodeInvalidRequest, "empty batch"))
		}
		var out []*Response
		for _, m := range batch {
			if r := s.handleOne(ctx, m); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return mustJSON(out)
	}
	if r := s.handleOne(ctx, trimmed); r != nil {
		return mustJSON(r)
	}
	return nil
}

func (s *Server) handleOne(ctx context.Context, raw json.RawMessage) *Response {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return errResp(nil, CodeParseError, "parse error: "+err.Error())
	}
	if _, isMethod := probe["method"]; !isMethod {
		// A response or error from the client (we never send requests): ignore.
		if _, ok := probe["result"]; ok {
			return nil
		}
		if _, ok := probe["error"]; ok {
			return nil
		}
		return errResp(probe["id"], CodeInvalidRequest, "invalid request: no method")
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errResp(probe["id"], CodeInvalidRequest, "invalid request: "+err.Error())
	}
	if req.JSONRPC != "2.0" {
		if req.IsNotification() {
			return nil
		}
		return errResp(req.ID, CodeInvalidRequest, `jsonrpc must be "2.0"`)
	}
	result, rpcErr := s.dispatch(ctx, &req)
	if req.IsNotification() {
		return nil
	}
	if rpcErr != nil {
		return &Response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
	}
	return &Response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) dispatch(ctx context.Context, req *Request) (any, *RPCError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
			}
		}
		v := LatestVersion
		if slices.Contains(SupportedVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		caps := map[string]any{"tools": map[string]any{"listChanged": false}}
		if len(s.resources) > 0 {
			caps["resources"] = map[string]any{"listChanged": false, "subscribe": false}
		}
		res := map[string]any{
			"protocolVersion": v,
			"capabilities":    caps,
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}
		if s.instructions != "" {
			res["instructions"] = s.instructions
		}
		return res, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return nil, nil
	case "tools/list":
		tools := make([]Tool, 0, len(s.tools))
		for _, n := range s.ToolNames() {
			tools = append(tools, s.tools[n])
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
		}
		t, ok := s.tools[p.Name]
		if !ok {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		res, err := safeCall(ctx, t.Handler, p.Arguments)
		if err != nil {
			return ErrorResult(err.Error()), nil
		}
		if res == nil {
			res = TextResult("ok")
		}
		return res, nil
	case "resources/list":
		uris := make([]string, 0, len(s.resources))
		for u := range s.resources {
			uris = append(uris, u)
		}
		sort.Strings(uris)
		out := make([]Resource, 0, len(uris))
		for _, u := range uris {
			out = append(out, s.resources[u])
		}
		return map[string]any{"resources": out}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
		}
		r, ok := s.resources[p.URI]
		if !ok {
			return nil, &RPCError{Code: -32002, Message: "resource not found", Data: map[string]string{"uri": p.URI}}
		}
		text, err := r.Read(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
		}
		mime := r.MimeType
		if mime == "" {
			mime = "text/plain"
		}
		return map[string]any{"contents": []map[string]any{{"uri": r.URI, "mimeType": mime, "text": text}}}, nil
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: fmt.Sprintf("method %q not found", req.Method)}
}

// safeCall turns a panicking tool into an error result.
func safeCall(ctx context.Context, h Handler, args json.RawMessage) (res *ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return h(ctx, args)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(errResp(nil, CodeInternalError, err.Error()))
	}
	return b
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

// DecodeArgs unmarshals tool arguments strictly (unknown fields are an
// error the model can fix).
func DecodeArgs(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytesReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ErrNoScope is returned by tools that need a project scope the caller
// does not have.
var ErrNoScope = errors.New("this connection has no project scope")
