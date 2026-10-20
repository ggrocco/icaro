// Package events fans run status changes and log lines out to subscribers
// (the SSE endpoint, the CLI's --wait, the UI). One goroutine owns the
// subscriber set; publishers and subscribers talk to it over channels.
package events

import (
	"context"
)

// Event types.
const (
	TypeRun  = "run"  // run status changed
	TypeStep = "step" // step status changed
	TypeLog  = "log"  // one log line
)

// Event is one notification about a run.
type Event struct {
	Type    string `json:"type"`
	RunID   string `json:"run_id"`
	Step    string `json:"step,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Status  string `json:"status,omitempty"`
	Line    string `json:"line,omitempty"`
	// Offset is the log file size after Line was written, so a client
	// that read a backlog up to offset N skips lines with Offset <= N.
	Offset int64 `json:"offset,omitempty"`
}

// Subscription receives a run's events. C is closed when the subscription
// ends, either by Cancel or because the subscriber fell too far behind
// (the client should resync from the API and resubscribe).
type Subscription struct {
	C      <-chan Event
	cancel func()
}

// Cancel ends the subscription.
func (s *Subscription) Cancel() { s.cancel() }

type subReq struct {
	runID string
	ch    chan Event
}

// Bus is the broadcaster.
type Bus struct {
	pub   chan Event
	sub   chan subReq
	unsub chan chan Event
	done  chan struct{}
}

// Buffer is the per-subscriber backlog before it is dropped.
const Buffer = 1024

// New starts a bus that lives until ctx ends.
func New(ctx context.Context) *Bus {
	b := &Bus{pub: make(chan Event, 256), sub: make(chan subReq), unsub: make(chan chan Event), done: make(chan struct{})}
	go b.loop(ctx)
	return b
}

func (b *Bus) loop(ctx context.Context) {
	defer close(b.done)
	subs := map[string]map[chan Event]bool{}
	drop := func(runID string, ch chan Event) {
		if set, ok := subs[runID]; ok && set[ch] {
			delete(set, ch)
			close(ch)
			if len(set) == 0 {
				delete(subs, runID)
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			for _, set := range subs {
				for ch := range set {
					close(ch)
				}
			}
			return
		case r := <-b.sub:
			if subs[r.runID] == nil {
				subs[r.runID] = map[chan Event]bool{}
			}
			subs[r.runID][r.ch] = true
		case ch := <-b.unsub:
			for runID, set := range subs {
				if set[ch] {
					drop(runID, ch)
				}
			}
		case ev := <-b.pub:
			for ch := range subs[ev.RunID] {
				select {
				case ch <- ev:
				default:
					drop(ev.RunID, ch) // slow subscriber: resync
				}
			}
		}
	}
}

// Publish sends an event without blocking the caller for long.
func (b *Bus) Publish(ev Event) {
	if b == nil {
		return
	}
	select {
	case b.pub <- ev:
	case <-b.done:
	}
}

// Subscribe follows one run.
func (b *Bus) Subscribe(runID string) *Subscription {
	ch := make(chan Event, Buffer)
	select {
	case b.sub <- subReq{runID: runID, ch: ch}:
	case <-b.done:
		close(ch)
		return &Subscription{C: ch, cancel: func() {}}
	}
	return &Subscription{C: ch, cancel: func() {
		select {
		case b.unsub <- ch:
		case <-b.done:
		}
	}}
}
