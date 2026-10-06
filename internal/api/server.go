// Package api exposes the service over HTTP/JSON at /api/v1.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ggrocco/icaro/internal/service"
	"github.com/ggrocco/icaro/internal/store"
	"github.com/ggrocco/icaro/internal/workflow"
)

const maxBody = 1 << 20

// Server is the HTTP API.
type Server struct {
	svc *service.Service
	log *slog.Logger
	mux *http.ServeMux
}

// New builds the router.
func New(svc *service.Service, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{svc: svc, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the root handler with logging and recovery.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.logging(s.mux))
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("GET /api/v1/schema/workflow", s.handleSchema)

	m.Handle("GET /api/v1/workflows", s.auth(store.ScopeRead, s.handleListWorkflows))
	m.Handle("POST /api/v1/workflows", s.auth(store.ScopeWrite, s.handleApplyWorkflow))
	m.Handle("POST /api/v1/workflows/validate", s.auth(store.ScopeRead, s.handleValidateWorkflow))
	m.Handle("GET /api/v1/workflows/{name}", s.auth(store.ScopeRead, s.handleGetWorkflow))
	m.Handle("DELETE /api/v1/workflows/{name}", s.auth(store.ScopeWrite, s.handleDeleteWorkflow))
	m.Handle("POST /api/v1/workflows/{name}/enable", s.auth(store.ScopeWrite, s.handleSetStatus(store.WorkflowActive)))
	m.Handle("POST /api/v1/workflows/{name}/disable", s.auth(store.ScopeWrite, s.handleSetStatus(store.WorkflowDisabled)))
	m.Handle("GET /api/v1/workflows/{name}/versions", s.auth(store.ScopeRead, s.handleListVersions))
	m.Handle("GET /api/v1/workflows/{name}/versions/{version}", s.auth(store.ScopeRead, s.handleGetVersion))
	m.Handle("POST /api/v1/workflows/{name}/runs", s.auth(store.ScopeWrite, s.handleEnqueueRun))
	m.Handle("GET /api/v1/workflows/{name}/runs", s.auth(store.ScopeRead, s.handleListRuns))

	m.Handle("GET /api/v1/runs", s.auth(store.ScopeRead, s.handleListRuns))
	m.Handle("GET /api/v1/runs/{id}", s.auth(store.ScopeRead, s.handleGetRun))
	m.Handle("POST /api/v1/runs/{id}/cancel", s.auth(store.ScopeWrite, s.handleCancelRun))
	m.Handle("GET /api/v1/runs/{id}/steps/{idx}/logs", s.auth(store.ScopeRead, s.handleStepLogs))

	m.Handle("GET /api/v1/connections", s.auth(store.ScopeRead, s.handleListConnections))
	m.Handle("POST /api/v1/connections", s.auth(store.ScopeAdmin, s.handleUpsertConnection))
	m.Handle("DELETE /api/v1/connections/{name}", s.auth(store.ScopeAdmin, s.handleDeleteConnection))
}

type ctxKey int

const tokenKey ctxKey = 1

// TokenFrom returns the authenticated token for a request.
func TokenFrom(ctx context.Context) *store.APIToken {
	t, _ := ctx.Value(tokenKey).(*store.APIToken)
	return t
}

// auth enforces a bearer token with at least the given scope.
func (s *Server) auth(scope string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="icaro"`)
			s.writeError(w, service.ErrUnauthorized)
			return
		}
		tok, err := s.svc.Authenticate(r.Context(), strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")))
		if err != nil {
			s.writeError(w, err)
			return
		}
		if !service.ScopeAllows(tok.Scope, scope) {
			s.writeError(w, service.ErrForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), tokenKey, tok)))
	})
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "path", r.URL.Path)
				s.writeError(w, errors.New("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// errorBody is the JSON error envelope.
type errorBody struct {
	Error  string          `json:"error"`
	Issues workflow.Issues `json:"issues,omitempty"`
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := errorBody{Error: err.Error()}
	var ve *service.ValidationError
	var br *service.BadRequest
	switch {
	case errors.As(err, &ve):
		status = http.StatusUnprocessableEntity
		body.Error = "validation failed"
		body.Issues = ve.Issues
	case errors.As(err, &br):
		status = http.StatusBadRequest
	case errors.Is(err, service.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, service.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, service.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, service.ErrConflict):
		status = http.StatusConflict
	default:
		s.log.Error("request failed", "err", err)
		body.Error = "internal error"
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return &service.BadRequest{Msg: "invalid JSON body: " + err.Error()}
	}
	return nil
}

// readYAML accepts either a raw YAML body (text/yaml, application/x-yaml,
// text/plain) or a JSON body {"yaml": "..."}.
func readYAML(r *http.Request) (string, error) {
	ct := r.Header.Get("Content-Type")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(ct, "application/json") {
		var in struct {
			YAML string `json:"yaml"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return "", &service.BadRequest{Msg: "invalid JSON body: " + err.Error()}
		}
		if in.YAML == "" {
			return "", &service.BadRequest{Msg: `body must be {"yaml": "..."} or raw YAML`}
		}
		return in.YAML, nil
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", &service.BadRequest{Msg: "empty body"}
	}
	return string(body), nil
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
