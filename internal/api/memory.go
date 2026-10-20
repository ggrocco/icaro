package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"icaro/internal/mcp"
	"icaro/internal/memory"
	"icaro/internal/service"
	"icaro/internal/store"
)

// Headers the host MCP endpoint reads to scope a connection. They are
// trusted because /mcp is authenticated like the API: the caller is the
// local user (or a token holder), who may read every project anyway.
const (
	HeaderProject = "X-Icaro-Project"
	HeaderBranch  = "X-Icaro-Branch"
	HeaderAgent   = "X-Icaro-Agent"
)

var projectIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// RegisterMemory mounts the memory REST fallback under /api/v1/memory and
// the host MCP endpoint at /mcp (architecture §8, §10).
func RegisterMemory(s *Server, eng *memory.Engine, host *mcp.Server) {
	m := s.Mux()
	h := &memoryHandlers{s: s, eng: eng}
	m.Handle("POST /api/v1/memory", s.Auth(store.ScopeWrite, h.write))
	m.Handle("GET /api/v1/memory", s.Auth(store.ScopeRead, h.query))
	m.Handle("GET /api/v1/memory/handoffs", s.Auth(store.ScopeRead, h.handoffs))
	m.Handle("POST /api/v1/memory/ingest", s.Auth(store.ScopeWrite, h.ingest))
	m.Handle("POST /api/v1/memory/import", s.Auth(store.ScopeWrite, h.importExport))
	m.Handle("GET /api/v1/memory/{id}", s.Auth(store.ScopeRead, h.provenance))
	m.Handle("POST /api/v1/memory/{id}/accept", s.Auth(store.ScopeWrite, h.accept))
	if host != nil {
		mh := host.HTTPHandler(func(r *http.Request) (mcp.Caller, error) {
			tok := TokenFrom(r.Context())
			c := mcp.Caller{Principal: tok.Name, Scope: tok.Scope, Branch: r.Header.Get(HeaderBranch), Agent: r.Header.Get(HeaderAgent)}
			if p := r.Header.Get(HeaderProject); p != "" {
				if !projectIDRe.MatchString(p) {
					return mcp.Caller{}, fmt.Errorf("invalid %s header", HeaderProject)
				}
				c.ProjectID = p
			}
			return c, nil
		})
		for _, method := range []string{"POST", "GET", "DELETE"} {
			m.Handle(method+" /mcp", s.Auth(store.ScopeRead, mh.ServeHTTP))
		}
	}
}

type memoryHandlers struct {
	s   *Server
	eng *memory.Engine
}

// memoryError maps memory errors onto the API's error envelope.
func (h *memoryHandlers) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, memory.ErrNotFound):
		h.s.WriteError(w, fmt.Errorf("%w: %s", service.ErrNotFound, err.Error()))
	case errors.Is(err, memory.ErrConflict):
		h.s.WriteError(w, &service.BadRequest{Msg: err.Error()})
	case errors.Is(err, memory.ErrInvalid):
		h.s.WriteError(w, &service.BadRequest{Msg: err.Error()})
	default:
		h.s.WriteError(w, err)
	}
}

// scopeOf builds the scope of a REST request; the project is mandatory.
func scopeOf(project, branch, worktree, agent, sessionID string) (memory.Scope, error) {
	if !projectIDRe.MatchString(project) {
		return memory.Scope{}, &service.BadRequest{Msg: "project is required (a project id, see `icaro memory project`)"}
	}
	return memory.Scope{ProjectID: project, Branch: branch, Worktree: worktree, Agent: agent, SessionID: sessionID, Origin: memory.OriginAPI}, nil
}

func (h *memoryHandlers) write(w http.ResponseWriter, r *http.Request) {
	var in memory.RESTWrite
	if err := ReadJSON(r, &in); err != nil {
		h.s.WriteError(w, err)
		return
	}
	scope, err := scopeOf(in.Project, in.Branch, in.Worktree, in.Agent, in.SessionID)
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	if in.Origin == memory.OriginCLI {
		scope.Origin = memory.OriginCLI
	}
	m, err := h.eng.Write(r.Context(), scope, memory.WriteRequest{
		Kind: in.Kind, Title: in.Title, Body: in.Body, Source: in.Source, SourceLine: in.SourceLine,
		TTL: time.Duration(in.TTLSeconds) * time.Second, Supersedes: in.Supersedes, Metadata: in.Metadata,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, m)
}

func (h *memoryHandlers) query(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope, err := scopeOf(q.Get("project"), "", "", "", "")
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	mq := memory.Query{
		Text: q.Get("q"), Branch: q.Get("branch"), Agent: q.Get("agent"), SourcePrefix: q.Get("source"),
		Status: q.Get("status"), Limit: queryInt(r, "limit", 10),
	}
	for _, k := range strings.Split(q.Get("kind"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			mq.Kinds = append(mq.Kinds, k)
		}
	}
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			h.s.WriteError(w, &service.BadRequest{Msg: "since must be RFC 3339"})
			return
		}
		mq.Since = &t
	}
	res, err := h.eng.Query(r.Context(), scope, mq)
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, memory.QueryResponse{Scope: memory.Scope{ProjectID: scope.ProjectID}, Count: len(res), Results: res, Timestamp: time.Now().UTC()})
}

func (h *memoryHandlers) handoffs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope, err := scopeOf(q.Get("project"), "", "", "", "")
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	res, err := h.eng.PendingHandoffs(r.Context(), scope, q.Get("branch"))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, memory.QueryResponse{Scope: memory.Scope{ProjectID: scope.ProjectID}, Count: len(res), Results: res, Timestamp: time.Now().UTC()})
}

func (h *memoryHandlers) provenance(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeOf(r.URL.Query().Get("project"), "", "", "", "")
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	p, err := h.eng.Provenance(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, p)
}

func (h *memoryHandlers) accept(w http.ResponseWriter, r *http.Request) {
	var in memory.RESTAccept
	if err := ReadJSON(r, &in); err != nil {
		h.s.WriteError(w, err)
		return
	}
	scope, err := scopeOf(in.Project, "", "", in.Agent, in.SessionID)
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	scope.Origin = memory.OriginCLI
	m, err := h.eng.AcceptHandoff(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, m)
}

func (h *memoryHandlers) ingest(w http.ResponseWriter, r *http.Request) {
	var in memory.RESTIngest
	if err := ReadJSON(r, &in); err != nil {
		h.s.WriteError(w, err)
		return
	}
	if in.Path == "" {
		h.s.WriteError(w, &service.BadRequest{Msg: "path is required"})
		return
	}
	if in.Project == "" {
		id, err := memory.ProjectIDForPath(in.Path)
		if err != nil {
			h.s.WriteError(w, &service.BadRequest{Msg: err.Error()})
			return
		}
		in.Project = id
	}
	scope, err := scopeOf(in.Project, "", "", "", "")
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	scope.Origin = memory.OriginIngest
	rep, err := h.eng.Ingest(r.Context(), scope, in.Path, memory.IngestOptions{Commits: in.Commits})
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"project": in.Project, "report": rep})
}

func (h *memoryHandlers) importExport(w http.ResponseWriter, r *http.Request) {
	var in memory.RESTImport
	// Exports can be larger than the default 1 MiB JSON cap.
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.s.WriteError(w, &service.BadRequest{Msg: "import body exceeds 32 MiB"})
		return
	}
	if err := decodeMemoryJSON(body, &in); err != nil {
		h.s.WriteError(w, err)
		return
	}
	scope, err := scopeOf(in.Project, "", "", "", "")
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	rep, err := h.eng.Import(r.Context(), scope, in.Format, []byte(in.Data))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, rep)
}

func decodeMemoryJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &service.BadRequest{Msg: "invalid JSON body: " + err.Error()}
	}
	return nil
}
