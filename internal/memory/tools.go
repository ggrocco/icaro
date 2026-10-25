package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"icaro/internal/mcp"
	"icaro/internal/session"
)

// Tool names (architecture §8: exactly three).
const (
	ToolWrite      = "memory_write"
	ToolQuery      = "memory_query"
	ToolProvenance = "memory_provenance"
)

// ScopeFromCaller derives the memory scope from an authenticated MCP
// caller. Tool arguments never contribute to it.
func ScopeFromCaller(c mcp.Caller) Scope {
	return Scope{ProjectID: c.ProjectID, Branch: c.Branch, Worktree: c.Worktree, Agent: c.Agent, SessionID: c.SessionID, Origin: OriginAgent}
}

func callerScope(ctx context.Context) (Scope, mcp.Caller, error) {
	c, ok := mcp.CallerFrom(ctx)
	if !ok || c.ProjectID == "" {
		return Scope{}, c, fmt.Errorf("%w: start the MCP bridge inside a repository (icaro mcp --project <path>) or from an Ícaro session", mcp.ErrNoScope)
	}
	return ScopeFromCaller(c), c, nil
}

const kindsEnum = `["decision","task","finding","handoff"]`

var (
	writeSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["kind", "title"],
  "properties": {
    "kind": {"type": "string", "enum": ` + kindsEnum + `, "description": "decision: a choice and its rationale; task: work to do; finding: something learned; handoff: state for the next agent/session (stays pending until accepted)."},
    "title": {"type": "string", "maxLength": 300},
    "body": {"type": "string", "description": "Details, rationale, next steps (markdown)."},
    "source": {"type": "string", "description": "Where this comes from: a repo-relative path, URL or commit. Defaults to this session."},
    "source_line": {"type": "integer", "minimum": 0},
    "ttl_seconds": {"type": "integer", "minimum": 1, "description": "Expire the memory after this many seconds."},
    "supersedes": {"type": "string", "description": "Id of an earlier memory this one replaces."},
    "metadata": {"type": "object"}
  }
}`)
	querySchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "text": {"type": "string", "description": "Full-text query; omit to list the newest memories."},
    "kinds": {"type": "array", "items": {"type": "string", "enum": ["decision","task","finding","handoff","file","symbol","adr","commit"]}},
    "branch": {"type": "string", "description": "Only this branch plus project-wide memories."},
    "agent": {"type": "string"},
    "since": {"type": "string", "format": "date-time"},
    "source_prefix": {"type": "string", "description": "Only memories whose source starts with this (e.g. internal/api/)."},
    "status": {"type": "string", "enum": ["pending", "accepted"], "description": "Handoff status."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100}
  }
}`)
	provenanceSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": {"id": {"type": "string"}, "accept": {"type": "boolean", "description": "For a pending handoff: mark it accepted by this session."}}
}`)
)

// QueryResponse is the memory_query result: every memory carries its
// source, timestamp and scope.
type QueryResponse struct {
	Scope     Scope     `json:"scope"`
	Count     int       `json:"count"`
	Results   []*Memory `json:"results"`
	Timestamp time.Time `json:"timestamp"`
}

// RegisterTools adds the three memory tools to s.
func RegisterTools(s *mcp.Server, e *Engine) {
	s.AddTool(mcp.Tool{
		Name:        ToolWrite,
		Title:       "Write project memory",
		Description: "Record a decision, task, finding or handoff in this project's shared memory, visible to every agent and session working on the project. Scope (project, branch, agent, session) is set by Ícaro, not by arguments.",
		InputSchema: writeSchema,
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), DestructiveHint: mcp.Bool(false), IdempotentHint: mcp.Bool(false), OpenWorldHint: mcp.Bool(false)},
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			scope, caller, err := callerScope(ctx)
			if err != nil {
				return nil, err
			}
			if caller.Scope == "read" {
				return nil, errors.New("this token is read-only")
			}
			var in struct {
				Kind       string         `json:"kind"`
				Title      string         `json:"title"`
				Body       string         `json:"body"`
				Source     string         `json:"source"`
				SourceLine int            `json:"source_line"`
				TTLSeconds int64          `json:"ttl_seconds"`
				Supersedes string         `json:"supersedes"`
				Metadata   map[string]any `json:"metadata"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			m, err := e.Write(ctx, scope, WriteRequest{
				Kind: in.Kind, Title: in.Title, Body: in.Body, Source: in.Source, SourceLine: in.SourceLine,
				TTL: time.Duration(in.TTLSeconds) * time.Second, Supersedes: in.Supersedes, Metadata: in.Metadata,
			})
			if err != nil {
				return nil, err
			}
			return mcp.JSONResult(m), nil
		},
	})
	s.AddTool(mcp.Tool{
		Name:        ToolQuery,
		Title:       "Search project memory",
		Description: "Search this project's memory: decisions, tasks, findings, handoffs written by agents and humans, plus indexed files, symbols, ADRs and commits. Every result includes its source and timestamp. Call with kinds=[\"handoff\"], status=\"pending\" before starting work.",
		InputSchema: querySchema,
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true), OpenWorldHint: mcp.Bool(false)},
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			scope, _, err := callerScope(ctx)
			if err != nil {
				return nil, err
			}
			var in struct {
				Text         string   `json:"text"`
				Kinds        []string `json:"kinds"`
				Branch       string   `json:"branch"`
				Agent        string   `json:"agent"`
				Since        string   `json:"since"`
				SourcePrefix string   `json:"source_prefix"`
				Status       string   `json:"status"`
				Limit        int      `json:"limit"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			q := Query{Text: in.Text, Kinds: in.Kinds, Branch: in.Branch, Agent: in.Agent, SourcePrefix: in.SourcePrefix, Status: in.Status, Limit: in.Limit}
			if in.Since != "" {
				t, err := time.Parse(time.RFC3339, in.Since)
				if err != nil {
					return nil, fmt.Errorf("since must be RFC 3339, e.g. 2026-01-02T15:04:05Z")
				}
				q.Since = &t
			}
			res, err := e.Query(ctx, scope, q)
			if err != nil {
				return nil, err
			}
			return mcp.JSONResult(QueryResponse{Scope: publicScope(scope), Count: len(res), Results: res, Timestamp: e.now()}), nil
		},
	})
	s.AddTool(mcp.Tool{
		Name:        ToolProvenance,
		Title:       "Read memory provenance",
		Description: "Return one memory in full with its provenance: origin, scope, source, link, timestamps and edges. Pass accept=true to accept a pending handoff.",
		InputSchema: provenanceSchema,
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), DestructiveHint: mcp.Bool(false), IdempotentHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			scope, caller, err := callerScope(ctx)
			if err != nil {
				return nil, err
			}
			var in struct {
				ID     string `json:"id"`
				Accept bool   `json:"accept"`
			}
			if err := mcp.DecodeArgs(args, &in); err != nil {
				return nil, err
			}
			if in.Accept {
				if caller.Scope == "read" {
					return nil, errors.New("this token is read-only")
				}
				if _, err := e.AcceptHandoff(ctx, scope, in.ID); err != nil {
					return nil, err
				}
			}
			p, err := e.Provenance(ctx, scope, in.ID)
			if err != nil {
				return nil, err
			}
			return mcp.JSONResult(p), nil
		},
	})
}

func publicScope(s Scope) Scope {
	s.Origin = ""
	return s
}

// NewMCPServer returns an MCP server exposing only the memory tools; it is
// what agents inside session containers see.
func (e *Engine) NewMCPServer(instructions string) *mcp.Server {
	s := mcp.NewServer("icaro-memory", e.version, instructions)
	RegisterTools(s, e)
	return s
}

// SetSessionInstructions sets the instructions (the handoff preamble)
// that per-session servers send on initialize. Call it before serving.
func (e *Engine) SetSessionInstructions(instructions string) {
	e.sessionServer = e.NewMCPServer(instructions)
}

// SessionServer is the MCP server used for per-session sockets.
func (e *Engine) SessionServer() *mcp.Server { return e.sessionServer }

// ServeSession implements session.MemoryAttacher: it serves the memory MCP
// server for one session on a 0600 Unix socket at socketPath, speaking
// streamable HTTP at / and /mcp. The scope is id; nothing a client sends
// can change it. stop closes the listener and removes the socket; so does
// cancelling ctx.
func (e *Engine) ServeSession(ctx context.Context, id session.Identity, socketPath string) (func(), error) {
	if id.ProjectID == "" {
		return nil, fmt.Errorf("%w: session %s has no project", ErrInvalid, id.SessionID)
	}
	if len(socketPath) >= MaxSocketPath {
		return nil, fmt.Errorf("%w: socket path %q is %d bytes; Unix sockets need < %d (use a shorter ICARO_HOME)", ErrInvalid, socketPath, len(socketPath), MaxSocketPath)
	}
	srv := e.sessionServer
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	caller := mcp.Caller{
		ProjectID: id.ProjectID, Branch: id.Branch, Agent: id.Agent, SessionID: id.SessionID,
		Principal: "session:" + id.SessionID, Scope: "write",
	}
	h := srv.HTTPHandler(func(*http.Request) (mcp.Caller, error) { return caller, nil })
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.Handle("/", h)
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()

	var once sync.Once
	done := make(chan struct{})
	stop := func() {
		once.Do(func() {
			close(done)
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = hs.Shutdown(sctx)
			_ = os.Remove(socketPath)
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	return stop, nil
}

// MaxSocketPath is the portable limit for a Unix socket path (macOS
// sun_path is 104 bytes including the terminator).
const MaxSocketPath = 104

var _ session.MemoryAttacher = (*Engine)(nil)
