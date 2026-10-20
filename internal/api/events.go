package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"icaro/internal/service"
	"icaro/internal/workflow/events"
)

// sseKeepAlive is how often an idle stream sends a comment.
var sseKeepAlive = 15 * time.Second

// sseBacklog caps the log backlog sent per step when a client connects.
const sseBacklog = 256 << 10

// handleRunEvents streams a run as Server-Sent Events (one stream per run):
//
//	event: snapshot   data: RunInfo            (first, and after each status change)
//	event: step       data: events.Event       (step status changes)
//	event: log        data: events.Event       (one line; with ?logs=1)
//	event: resync     data: {}                 (the client fell behind; reconnect)
//	event: end        data: RunInfo            (terminal; the stream closes)
//
// Log events carry the file offset after the line, so the backlog sent on
// connect and the live stream never duplicate a line.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.svc.GetRun(r.Context(), id); err != nil {
		s.writeError(w, err)
		return
	}
	sub, err := s.svc.SubscribeRun(id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer sub.Cancel()
	// Re-read after subscribing so no transition falls in the gap.
	run, err := s.svc.GetRun(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("snapshot", run) {
		return
	}

	withLogs := r.URL.Query().Get("logs") == "1"
	seen := map[string]int64{} // step/attempt -> backlog offset
	if withLogs {
		for _, st := range run.Steps {
			if st.Attempt == 0 {
				continue
			}
			page, err := s.svc.StepLogs(r.Context(), id, st.ID, st.Attempt, 0, sseBacklog)
			if err != nil {
				continue
			}
			key := fmt.Sprintf("%s/%d", st.ID, st.Attempt)
			seen[key] = page.Offset + int64(len(page.Data))
			off := page.Offset
			for _, line := range bytes.SplitAfter(page.Data, []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				off += int64(len(line))
				if !send(events.TypeLog, events.Event{Type: events.TypeLog, RunID: id, Step: st.ID, Attempt: st.Attempt,
					Line: string(bytes.TrimRight(line, "\r\n")), Offset: off}) {
					return
				}
			}
		}
	}
	if service.Terminal(run.Status) {
		send("end", run)
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
		case ev, ok := <-sub.C:
			if !ok {
				send("resync", map[string]any{})
				return
			}
			switch ev.Type {
			case events.TypeLog:
				if !withLogs || ev.Offset <= seen[fmt.Sprintf("%s/%d", ev.Step, ev.Attempt)] {
					continue
				}
				if !send(events.TypeLog, ev) {
					return
				}
			case events.TypeStep:
				if !send(events.TypeStep, ev) {
					return
				}
			case events.TypeRun:
				cur, err := s.svc.GetRun(r.Context(), id)
				if err != nil {
					return
				}
				if service.Terminal(cur.Status) {
					send("end", cur)
					return
				}
				if !send("snapshot", cur) {
					return
				}
			}
		}
	}
}
