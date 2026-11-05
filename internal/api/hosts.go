package api

import (
	"errors"
	"fmt"
	"net/http"

	"icaro/internal/hosts"
	"icaro/internal/service"
	"icaro/internal/store"
)

// RegisterHosts mounts the remote host routes (ADR 0033). Signing in and
// out stores and forgets other daemons' tokens, and makes this daemon call
// a URL of the caller's choosing, so both need admin.
func RegisterHosts(s *Server, reg *hosts.Registry) {
	h := &hostHandlers{s: s, reg: reg}
	mux := s.Mux()
	mux.Handle("GET /api/v1/hosts", s.Auth(store.ScopeRead, h.list))
	mux.Handle("POST /api/v1/hosts", s.Auth(store.ScopeAdmin, h.login))
	mux.Handle("DELETE /api/v1/hosts/{alias}", s.Auth(store.ScopeAdmin, h.logout))
	mux.Handle("GET /api/v1/hosts/workflows", s.Auth(store.ScopeRead, h.workflows))
}

type hostHandlers struct {
	s   *Server
	reg *hosts.Registry
}

// fail maps host errors: a bad request or a refused token is the caller's
// to fix (400), an unreachable host a 502.
func (h *hostHandlers) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hosts.ErrNotFound):
		h.s.WriteError(w, fmt.Errorf("%w: %s", service.ErrNotFound, err.Error()))
	case errors.Is(err, hosts.ErrInvalid), errors.Is(err, hosts.ErrRefused):
		h.s.WriteError(w, &service.BadRequest{Msg: err.Error()})
	case errors.Is(err, hosts.ErrUnreachable):
		h.s.WriteError(w, &service.Upstream{Msg: err.Error()})
	default:
		h.s.WriteError(w, err)
	}
}

func (h *hostHandlers) list(w http.ResponseWriter, r *http.Request) {
	hs, err := h.reg.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"hosts": hs})
}

func (h *hostHandlers) login(w http.ResponseWriter, r *http.Request) {
	var in hosts.Login
	if err := ReadJSON(r, &in); err != nil {
		h.s.WriteError(w, err)
		return
	}
	host, err := h.reg.Login(r.Context(), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, host)
}

func (h *hostHandlers) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.reg.Logout(r.Context(), r.PathValue("alias")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hostHandlers) workflows(w http.ResponseWriter, r *http.Request) {
	out, err := h.reg.Workflows(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}
