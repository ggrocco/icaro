package assist

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"icaro/internal/assist/stream"
	"icaro/internal/logs"
	"icaro/internal/store"
	"icaro/internal/workflow/events"
)

// Log entry types.
const (
	// EntryMessage is a new message.
	EntryMessage = "message"
	// EntryStatus is a turn's status change, or a note about the chat's
	// container.
	EntryStatus = "status"
	// EntryEvent is one normalized event of a turn's stream.
	EntryEvent = "event"
	// EntryDraft is a new draft version.
	EntryDraft = "draft"
	// EntryApproval is a gated call that waits or was decided.
	EntryApproval = "approval"
)

// DraftEntry describes a draft change in the log; clients fetch the draft.
type DraftEntry struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	Issues  int    `json:"issues"`
}

// entry is one line of a chat's log.
type entry struct {
	Time      time.Time     `json:"time"`
	Type      string        `json:"type"`
	MessageID string        `json:"message_id,omitempty"`
	Status    string        `json:"status,omitempty"`
	Note      string        `json:"note,omitempty"`
	Message   *MessageInfo  `json:"message,omitempty"`
	Event     *stream.Event `json:"event,omitempty"`
	Draft     *DraftEntry   `json:"draft,omitempty"`
	Approval  *ApprovalInfo `json:"approval,omitempty"`
}

// job is one queued turn.
type job struct {
	messageID string
	text      string
	actor     string
}

// running is the turn in flight.
type running struct {
	job    job
	cancel context.CancelFunc
}

// actor owns one chat's log file, its queue of turns and its idle timer.
// Its state is touched only by its goroutine; callers send closures.
type actor struct {
	s      *Service
	chatID string
	in     chan func(*actorState)
	done   chan struct{}
}

type actorState struct {
	f       *os.File
	size    int64
	opened  bool
	redact  *redactor
	queue   []job
	running *running
	idle    *time.Timer
	closing bool
}

func newActor(s *Service, chatID string) *actor {
	a := &actor{s: s, chatID: chatID, in: make(chan func(*actorState)), done: make(chan struct{})}
	go a.loop()
	return a
}

func (a *actor) loop() {
	st := &actorState{}
	defer close(a.done)
	for fn := range a.in {
		fn(st)
		if st.closing && st.running == nil {
			if st.idle != nil {
				st.idle.Stop()
			}
			if st.f != nil {
				_ = st.f.Close()
			}
			return
		}
	}
}

// do runs fn on the actor's goroutine; it reports false once the actor
// has shut down.
func (a *actor) do(fn func(*actorState)) bool {
	done := make(chan struct{})
	select {
	case a.in <- func(st *actorState) { fn(st); close(done) }:
		<-done
		return true
	case <-a.done:
		return false
	}
}

// shutdown cancels the running turn, drops the queue, and waits for the
// turn to finish and the actor to exit.
func (a *actor) shutdown() {
	a.do(func(st *actorState) {
		st.closing = true
		st.queue = nil
		if st.running != nil {
			st.running.cancel()
		}
	})
	<-a.done
}

// size returns the log's current size.
func (a *actor) size() int64 {
	var n int64
	a.do(func(st *actorState) {
		a.open(st)
		n = st.size
	})
	return n
}

// append writes e to the log and rings the chat's doorbell.
func (a *actor) append(e entry) {
	a.do(func(st *actorState) { a.write(st, e) })
}

func (a *actor) open(st *actorState) {
	if st.opened {
		return
	}
	st.opened = true
	if err := os.MkdirAll(a.s.o.LogsDir, 0o700); err != nil {
		a.s.o.Log.Error("assistant: log directory", "err", err)
		return
	}
	f, err := os.OpenFile(a.s.logPath(a.chatID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.s.o.Log.Error("assistant: open chat log", "chat", a.chatID, "err", err)
		return
	}
	if fi, err := f.Stat(); err == nil {
		st.size = fi.Size()
	}
	st.f = f
}

func (a *actor) write(st *actorState, e entry) {
	a.open(st)
	if st.f == nil {
		return
	}
	if st.redact == nil {
		st.redact = a.s.redactor(a.s.base)
	}
	e.Time = a.s.o.Now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	// Secrets are redacted as the log is written, like every other log.
	line := append(st.redact.Redact(b), '\n')
	if !json.Valid(line[:len(line)-1]) {
		return
	}
	n, err := st.f.Write(line)
	st.size += int64(n)
	if err != nil {
		a.s.o.Log.Error("assistant: write chat log", "chat", a.chatID, "err", err)
		return
	}
	a.s.o.Events.Publish(events.Event{Type: "chat", RunID: Topic(a.chatID), Offset: st.size})
}

// enqueue adds a turn and starts it when nothing runs.
func (a *actor) enqueue(j job) {
	a.do(func(st *actorState) {
		st.queue = append(st.queue, j)
		a.next(st)
	})
}

// next starts the first queued turn when none runs.
func (a *actor) next(st *actorState) {
	if st.running != nil || len(st.queue) == 0 || st.closing {
		return
	}
	j := st.queue[0]
	st.queue = st.queue[1:]
	if st.idle != nil {
		st.idle.Stop()
		st.idle = nil
	}
	ctx, cancel := context.WithCancel(a.s.base)
	st.running = &running{job: j, cancel: cancel}
	st.redact = a.s.redactor(ctx) // pick up secrets set since the last turn
	go func() {
		defer cancel()
		a.s.runTurn(ctx, a, j)
		a.finished()
	}()
}

// finished records that the running turn ended.
func (a *actor) finished() {
	a.do(func(st *actorState) {
		st.running = nil
		if st.closing {
			return
		}
		if len(st.queue) > 0 {
			a.next(st)
			return
		}
		st.idle = time.AfterFunc(a.s.o.Config.Assistant.IdleTimeout, a.idleExpired)
	})
}

// idleExpired stops the chat's container after a quiet period.
func (a *actor) idleExpired() {
	stop := false
	a.do(func(st *actorState) {
		st.idle = nil
		stop = st.running == nil && len(st.queue) == 0 && !st.closing
	})
	if !stop {
		return
	}
	ctx := a.s.base
	c, err := a.s.o.Store.GetChat(ctx, a.chatID)
	if err != nil || c.SessionID == "" {
		return
	}
	if _, err := a.s.o.Sessions.Stop(ctx, c.SessionID); err != nil {
		a.s.o.Log.Warn("assistant: stop idle container", "chat", a.chatID, "err", err)
		return
	}
	a.append(entry{Type: EntryStatus, Note: "The assistant's container stopped after a quiet period; the next message starts it again."})
}

// cancel stops the running turn and cancels the queued ones.
func (a *actor) cancel() {
	var dropped []job
	a.do(func(st *actorState) {
		dropped, st.queue = st.queue, nil
		if st.running != nil {
			st.running.cancel()
		}
	})
	ctx := a.s.base
	for _, j := range dropped {
		m, err := a.s.o.Store.GetChatMessage(ctx, j.messageID)
		if err != nil {
			continue
		}
		now := a.s.o.Now()
		m.Status, m.Error, m.FinishedAt = store.TurnCancelled, "cancelled before it started", &now
		if ok, _ := a.s.o.Store.UpdateChatMessageIf(ctx, m, store.TurnQueued); ok {
			a.append(entry{Type: EntryStatus, MessageID: m.ID, Status: m.Status})
		}
	}
}

// redactor redacts every known secret value and every well-known token
// format (logs.RedactString).
type redactor struct{ values []string }

func (r *redactor) Redact(b []byte) []byte { return []byte(r.RedactString(string(b))) }

func (r *redactor) RedactString(s string) string { return logs.RedactString(s, r.values) }

func (s *Service) redactor(ctx context.Context) *redactor {
	r := &redactor{}
	if s.o.Secrets != nil {
		if v, err := s.o.Secrets.Values(ctx); err == nil {
			r.values = v
		}
	}
	return r
}
