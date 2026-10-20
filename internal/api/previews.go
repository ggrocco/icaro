package api

import (
	"errors"
	"net/http"
	"strconv"

	"icaro/internal/preview"
	"icaro/internal/service"
	"icaro/internal/store"
)

// PreviewInfo is the API view of a preview.
type PreviewInfo struct {
	*store.PreviewInstance
	Curl string `json:"curl,omitempty"`
}

type previewPathBody struct {
	Path  string `json:"path"`
	Force bool   `json:"force,omitempty"`
}

type previewAllowBody struct {
	Path     string   `json:"path"`
	Findings []string `json:"findings"`
	// Confirm must be true: allowing a finding weakens the compose lint.
	Confirm bool `json:"confirm"`
}

// RegisterPreviews mounts the preview routes.
func RegisterPreviews(s *Server, m *preview.Manager) {
	h := &previewHandlers{s: s, m: m}
	mux := s.Mux()
	mux.Handle("GET /api/v1/previews", s.Auth(store.ScopeRead, h.list))
	mux.Handle("GET /api/v1/previews/{id}", s.Auth(store.ScopeRead, h.get))
	mux.Handle("GET /api/v1/previews/{id}/logs", s.Auth(store.ScopeRead, h.logs))
	mux.Handle("POST /api/v1/previews/up", s.Auth(store.ScopeWrite, h.up))
	mux.Handle("POST /api/v1/previews/down", s.Auth(store.ScopeWrite, h.down))
	mux.Handle("POST /api/v1/previews/remove", s.Auth(store.ScopeWrite, h.remove))
	mux.Handle("POST /api/v1/previews/lint", s.Auth(store.ScopeRead, h.lint))
	mux.Handle("POST /api/v1/previews/allow", s.Auth(store.ScopeAdmin, h.allow))
}

type previewHandlers struct {
	s *Server
	m *preview.Manager
}

func (h *previewHandlers) info(p *store.PreviewInstance) PreviewInfo {
	return PreviewInfo{PreviewInstance: p, Curl: h.m.CurlHint(p)}
}

func (h *previewHandlers) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.s.WriteError(w, service.ErrNotFound)
	default:
		h.s.WriteError(w, &service.BadRequest{Msg: err.Error()})
	}
}

func (h *previewHandlers) readPath(w http.ResponseWriter, r *http.Request) (previewPathBody, bool) {
	var in previewPathBody
	if err := ReadJSON(r, &in); err != nil {
		h.s.WriteError(w, err)
		return in, false
	}
	if in.Path == "" {
		h.s.WriteError(w, &service.BadRequest{Msg: "path is required"})
		return in, false
	}
	return in, true
}

func (h *previewHandlers) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.m.List(r.Context())
	if err != nil {
		h.s.WriteError(w, err)
		return
	}
	out := make([]PreviewInfo, 0, len(list))
	for _, p := range list {
		out = append(out, h.info(p))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"previews": out})
}

func (h *previewHandlers) get(w http.ResponseWriter, r *http.Request) {
	p, err := h.m.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, h.info(p))
}

func (h *previewHandlers) logs(w http.ResponseWriter, r *http.Request) {
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if _, err := h.m.Get(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := h.m.Logs(r.Context(), r.PathValue("id"), tail, w); err != nil {
		_, _ = w.Write([]byte("\n(logs unavailable: " + err.Error() + ")\n"))
	}
}

func (h *previewHandlers) up(w http.ResponseWriter, r *http.Request) {
	in, ok := h.readPath(w, r)
	if !ok {
		return
	}
	p, err := h.m.Add(r.Context(), preview.AddRequest{Path: in.Path, Wait: true, Force: in.Force})
	if p == nil {
		h.fail(w, err)
		return
	}
	// A preview that failed to start is still a result: its status and
	// error say why.
	WriteJSON(w, http.StatusOK, h.info(p))
}

func (h *previewHandlers) down(w http.ResponseWriter, r *http.Request) {
	in, ok := h.readPath(w, r)
	if !ok {
		return
	}
	p, err := h.m.Down(r.Context(), in.Path)
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, h.info(p))
}

func (h *previewHandlers) remove(w http.ResponseWriter, r *http.Request) {
	in, ok := h.readPath(w, r)
	if !ok {
		return
	}
	if err := h.m.Remove(r.Context(), in.Path); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *previewHandlers) lint(w http.ResponseWriter, r *http.Request) {
	in, ok := h.readPath(w, r)
	if !ok {
		return
	}
	res, err := h.m.Lint(r.Context(), in.Path)
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

func (h *previewHandlers) allow(w http.ResponseWriter, r *http.Request) {
	var in previewAllowBody
	if err := ReadJSON(r, &in); err != nil {
		h.s.WriteError(w, err)
		return
	}
	if in.Path == "" || len(in.Findings) == 0 {
		h.s.WriteError(w, &service.BadRequest{Msg: "path and findings are required"})
		return
	}
	if !in.Confirm {
		h.s.WriteError(w, &service.BadRequest{Msg: "allowing a compose-lint finding needs explicit confirmation (confirm: true / --yes)"})
		return
	}
	actor := "unknown"
	if t := TokenFrom(r.Context()); t != nil {
		actor = t.Name
	}
	if err := h.m.Allow(r.Context(), in.Path, in.Findings, actor); err != nil {
		h.fail(w, err)
		return
	}
	res, err := h.m.Lint(r.Context(), in.Path)
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}
