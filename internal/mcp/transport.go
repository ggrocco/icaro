package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// maxMessage bounds one JSON-RPC message on any transport.
const maxMessage = 4 << 20

// ServeStdio reads newline-delimited JSON-RPC messages from r and writes
// replies to w until r ends or ctx is cancelled. Every message runs as
// caller.
func (s *Server) ServeStdio(ctx context.Context, caller Caller, r io.Reader, w io.Writer) error {
	ctx = WithCaller(ctx, caller)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if out := s.HandleMessage(ctx, sc.Bytes()); out != nil {
			if _, err := w.Write(append(out, '\n')); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// SessionHeader carries the streamable HTTP session id.
const SessionHeader = "Mcp-Session-Id"

// ProtocolHeader carries the negotiated protocol version.
const ProtocolHeader = "MCP-Protocol-Version"

// CallerFunc authenticates an HTTP request into a Caller.
type CallerFunc func(r *http.Request) (Caller, error)

// HTTPHandler serves the streamable HTTP transport: POST carries one
// JSON-RPC message (or batch) and gets a JSON reply; GET (server-sent
// stream) is not offered; DELETE ends a session. The handler is
// stateless: session ids are issued on initialize and echoed, but every
// request is authenticated on its own.
func (s *Server) HTTPHandler(callerFn CallerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet:
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "this server does not offer a server-initiated stream", http.StatusMethodNotAllowed)
			return
		default:
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if v := r.Header.Get(ProtocolHeader); v != "" && !slices.Contains(SupportedVersions, v) {
			http.Error(w, fmt.Sprintf("unsupported %s %q", ProtocolHeader, v), http.StatusBadRequest)
			return
		}
		caller, err := callerFn(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxMessage+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxMessage {
			http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
			return
		}
		out := s.HandleMessage(WithCaller(r.Context(), caller), body)
		if r.Header.Get(SessionHeader) == "" && isInitialize(body) {
			w.Header().Set(SessionHeader, newSessionID())
		} else if id := r.Header.Get(SessionHeader); id != "" {
			w.Header().Set(SessionHeader, id)
		}
		if out == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
}

func isInitialize(body []byte) bool {
	var probe struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Method == "initialize"
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Bridge copies newline-delimited JSON-RPC from in to a streamable HTTP
// endpoint and writes the replies to out, one per line. It is the stdio
// side that agent CLIs launch (`icaro mcp`).
type Bridge struct {
	Client *http.Client
	URL    string
	Header http.Header

	sessionID string
}

// Run bridges until in ends or ctx is cancelled. A transport failure on
// one request is answered with a JSON-RPC error so the client does not
// hang waiting for a reply.
func (b *Bridge) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	for sc.Scan() {
		line := trimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		reply, err := b.Post(ctx, line)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			reply = transportError(line, err)
		}
		if reply == nil {
			continue
		}
		var compact bytes.Buffer
		if json.Compact(&compact, reply) == nil {
			reply = compact.Bytes()
		}
		if _, err := out.Write(append(reply, '\n')); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Post sends one message and returns the reply body (nil for 202).
func (b *Bridge) Post(ctx context.Context, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	for k, vs := range b.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.sessionID != "" {
		req.Header.Set(SessionHeader, b.sessionID)
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	if err != nil {
		return nil, err
	}
	if id := resp.Header.Get(SessionHeader); id != "" {
		b.sessionID = id
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func transportError(msg []byte, err error) []byte {
	var req Request
	if json.Unmarshal(msg, &req) != nil || req.IsNotification() {
		return nil
	}
	return mustJSON(errResp(req.ID, CodeInternalError, "icaro bridge: "+err.Error()))
}

// Client is a minimal MCP client for tests and tooling.
type Client struct {
	// Send delivers one message and returns the reply (nil for none).
	Send func(ctx context.Context, msg []byte) ([]byte, error)
	next int
}

// InProcess returns a client that calls s directly as caller.
func InProcess(s *Server, caller Caller) *Client {
	return &Client{Send: func(ctx context.Context, msg []byte) ([]byte, error) {
		return s.HandleMessage(WithCaller(ctx, caller), msg), nil
	}}
}

// HTTPClient returns a client over a streamable HTTP endpoint.
func HTTPClient(hc *http.Client, url string, header http.Header) *Client {
	b := &Bridge{Client: hc, URL: url, Header: header}
	return &Client{Send: b.Post}
}

// Call performs a request and decodes its result into out.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	c.next++
	msg := map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, _ := json.Marshal(msg)
	reply, err := c.Send(ctx, raw)
	if err != nil {
		return err
	}
	if reply == nil {
		return errors.New("mcp: no reply")
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(reply, &resp); err != nil {
		return fmt.Errorf("mcp: decode reply: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// Notify sends a notification.
func (c *Client) Notify(ctx context.Context, method string) error {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, err := c.Send(ctx, raw)
	return err
}

// InitializeResult is the decoded initialize reply.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// Initialize runs the handshake.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	var res InitializeResult
	if err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": LatestVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "icaro-test", "version": "0"},
	}, &res); err != nil {
		return nil, err
	}
	return &res, c.Notify(ctx, "notifications/initialized")
}

// ListTools returns the tool list.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var res struct {
		Tools []Tool `json:"tools"`
	}
	err := c.Call(ctx, "tools/list", map[string]any{}, &res)
	return res.Tools, err
}

// CallTool calls a tool.
func (c *Client) CallTool(ctx context.Context, name string, args any) (*ToolResult, error) {
	var res struct {
		Content           []Content       `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	out := &ToolResult{Content: res.Content, IsError: res.IsError}
	if len(res.StructuredContent) > 0 {
		out.StructuredContent = res.StructuredContent
	}
	return out, nil
}

// Structured decodes a result's structured content into v.
func Structured(r *ToolResult, v any) error {
	switch sc := r.StructuredContent.(type) {
	case json.RawMessage:
		return json.Unmarshal(sc, v)
	case nil:
		if len(r.Content) > 0 {
			return json.Unmarshal([]byte(r.Content[0].Text), v)
		}
		return errors.New("mcp: empty result")
	default:
		b, err := json.Marshal(sc)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
}
