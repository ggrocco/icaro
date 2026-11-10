package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"icaro/internal/assist"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow/events"
)

// Chats is the workflow assistant (ADR 0035) as the API uses it.
type Chats interface {
	CreateChat(ctx context.Context, req assist.CreateRequest, actor string) (*assist.ChatDetail, error)
	ListChats(ctx context.Context) ([]assist.ChatInfo, error)
	GetChat(ctx context.Context, id string) (*assist.ChatDetail, error)
	Send(ctx context.Context, chatID, text, actor string) ([]assist.MessageInfo, error)
	Retry(ctx context.Context, chatID, messageID, actor string) ([]assist.MessageInfo, error)
	Cancel(ctx context.Context, chatID string) error
	PutDraft(ctx context.Context, chatID, yaml string) (*assist.DraftInfo, error)
	Decide(ctx context.Context, chatID, approvalID string, approve bool, actor string) (*assist.ApprovalInfo, error)
	Delete(ctx context.Context, chatID string) error
	Entries(ctx context.Context, chatID string, offset int64) ([]assist.LogLine, int64, error)
}

// RegisterChats mounts the assistant's endpoints. Reading and following a
// chat needs read scope; talking to it, editing its draft and deciding its
// gated calls need write scope (and, from a browser, the CSRF header).
func RegisterChats(s *Server, c Chats, bus *events.Bus) {
	h := &chatHandlers{s: s, c: c, bus: bus}
	m := s.Mux()
	m.Handle("GET /api/v1/chats", s.Auth(store.ScopeRead, h.list))
	m.Handle("POST /api/v1/chats", s.Auth(store.ScopeWrite, h.create))
	m.Handle("GET /api/v1/chats/{id}", s.Auth(store.ScopeRead, h.get))
	m.Handle("DELETE /api/v1/chats/{id}", s.Auth(store.ScopeWrite, h.remove))
	m.Handle("POST /api/v1/chats/{id}/messages", s.Auth(store.ScopeWrite, h.send))
	m.Handle("POST /api/v1/chats/{id}/messages/{msg}/retry", s.Auth(store.ScopeWrite, h.retry))
	m.Handle("POST /api/v1/chats/{id}/cancel", s.Auth(store.ScopeWrite, h.cancel))
	m.Handle("PUT /api/v1/chats/{id}/draft", s.Auth(store.ScopeWrite, h.draft))
	m.Handle("POST /api/v1/chats/{id}/approvals/{approval}/approve", s.Auth(store.ScopeWrite, h.decide(true)))
	m.Handle("POST /api/v1/chats/{id}/approvals/{approval}/decline", s.Auth(store.ScopeWrite, h.decide(false)))
	m.Handle("GET /api/v1/chats/{id}/events", s.Auth(store.ScopeRead, h.events))
}

type chatHandlers struct {
	s   *Server
	c   Chats
	bus *events.Bus
}

// chatError maps the assistant's errors onto the API's.
func chatError(err error) error {
	switch {
	case errors.Is(err, assist.ErrNotFound):
		return fmt.Errorf("%w: %v", service.ErrNotFound, err)
	case errors.Is(err, assist.ErrInvalid):
		return &service.BadRequest{Msg: err.Error()}
	case errors.Is(err, assist.ErrBusy):
		return &service.Conflict{Msg: err.Error()}
	}
	return err
}

func (h *chatHandlers) fail(w http.ResponseWriter, err error) { h.s.WriteError(w, chatError(err)) }

func (h *chatHandlers) list(w http.ResponseWriter, r *http.Request) {
	out, err := h.c.ListChats(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"chats": out})
}

func (h *chatHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in assist.CreateRequest
	if err := ReadJSON(r, &in); err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.c.CreateChat(r.Context(), in, actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, d)
}

func (h *chatHandlers) get(w http.ResponseWriter, r *http.Request) {
	d, err := h.c.GetChat(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, d)
}

func (h *chatHandlers) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.c.Delete(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *chatHandlers) send(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := ReadJSON(r, &in); err != nil {
		h.fail(w, err)
		return
	}
	msgs, err := h.c.Send(r.Context(), r.PathValue("id"), in.Text, actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"messages": msgs})
}

func (h *chatHandlers) retry(w http.ResponseWriter, r *http.Request) {
	msgs, err := h.c.Retry(r.Context(), r.PathValue("id"), r.PathValue("msg"), actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"messages": msgs})
}

func (h *chatHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	if err := h.c.Cancel(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *chatHandlers) draft(w http.ResponseWriter, r *http.Request) {
	var in struct {
		YAML string `json:"yaml"`
	}
	if err := ReadJSON(r, &in); err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.c.PutDraft(r.Context(), r.PathValue("id"), in.YAML)
	if err != nil {
		h.fail(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, d)
}

func (h *chatHandlers) decide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who := actor(r)
		if who == "" {
			who = "someone"
		}
		a, err := h.c.Decide(r.Context(), r.PathValue("id"), r.PathValue("approval"), approve, who)
		if err != nil {
			h.fail(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, a)
	}
}

// events streams a chat's log as Server-Sent Events, from ?offset= (or
// the Last-Event-ID header a reconnecting browser sends):
//
//	event: entry   id: <offset after it>   data: one log entry
//	event: resync  data: {}   (the client fell behind; reconnect with the last id)
//
// The log is the source of truth; the bus only says it grew.
func (h *chatHandlers) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	offset := int64(0)
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("offset")} {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}
	if _, _, err := h.c.Entries(r.Context(), id, offset); err != nil {
		h.fail(w, err)
		return
	}
	sub := h.bus.Subscribe(assist.Topic(id))
	defer sub.Cancel()
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush := func() bool {
		for {
			entries, next, err := h.c.Entries(r.Context(), id, offset)
			if err != nil {
				return false
			}
			if len(entries) == 0 {
				return true
			}
			for _, e := range entries {
				// The id is the offset after the entry: a reconnecting
				// client resumes there and never sees an entry twice.
				if _, err := fmt.Fprintf(w, "event: entry\nid: %d\ndata: %s\n\n", e.Next, e.Data); err != nil {
					return false
				}
			}
			offset = next
			if rc.Flush() != nil {
				return false
			}
		}
	}
	if !flush() {
		return
	}
	keepAlive := time.NewTicker(sseKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case _, ok := <-sub.C:
			if !ok {
				_, _ = fmt.Fprintf(w, "event: resync\nid: %d\ndata: {}\n\n", offset)
				_ = rc.Flush()
				return
			}
			if !flush() {
				return
			}
		}
	}
}
