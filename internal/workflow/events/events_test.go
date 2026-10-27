package events

import (
	"context"
	"testing"
	"time"
)

func recv(t *testing.T, c <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-c:
		return ev, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	return Event{}, false
}

func TestBus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := New(ctx)
	s1 := b.Subscribe("r1")
	s2 := b.Subscribe("r2")
	b.Publish(Event{Type: TypeLog, RunID: "r1", Line: "hello"})
	b.Publish(Event{Type: TypeRun, RunID: "r2", Status: "running"})
	if ev, _ := recv(t, s1.C); ev.Line != "hello" {
		t.Fatalf("%+v", ev)
	}
	if ev, _ := recv(t, s2.C); ev.Status != "running" {
		t.Fatalf("%+v", ev)
	}
	s1.Cancel()
	if _, ok := recv(t, s1.C); ok {
		t.Fatal("cancelled subscription must close")
	}
	// A subscriber that falls behind is dropped instead of blocking the
	// publisher; whatever it received before is still delivered in order.
	total := 4 * Buffer
	for i := 0; i < total; i++ {
		b.Publish(Event{Type: TypeLog, RunID: "r2", Offset: int64(i)})
	}
	b.Publish(Event{Type: TypeRun, RunID: "r2", Status: "end"})
	n := 0
	for {
		ev, ok := recv(t, s2.C)
		if !ok || ev.Status == "end" {
			break
		}
		if ev.Offset != int64(n) {
			t.Fatalf("out of order: %d at %d", ev.Offset, n)
		}
		n++
	}
	if n == 0 {
		t.Fatal("received nothing")
	}
	var nilBus *Bus
	nilBus.Publish(Event{})
	cancel()
	s3 := b.Subscribe("r3")
	if _, ok := recv(t, s3.C); ok {
		t.Fatal("subscribe after shutdown returns a closed channel")
	}
	s3.Cancel()
}

// Stopping the bus closes every live subscription, so followers of a run
// (SSE streams) end instead of hanging.
func TestShutdownClosesSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := New(ctx)
	s1 := b.Subscribe("r1")
	s2 := b.Subscribe("r1")
	s3 := b.Subscribe("r2")
	cancel()
	for _, s := range []*Subscription{s1, s2, s3} {
		for {
			if _, ok := recv(t, s.C); !ok {
				break
			}
		}
		s.Cancel() // after shutdown, a no-op that does not block
	}
	b.Publish(Event{RunID: "r1"}) // does not block after shutdown
	// The loop has exited, so a new subscription is closed at once.
	late := b.Subscribe("r9")
	if _, ok := <-late.C; ok {
		t.Fatal("a subscription after shutdown must be closed")
	}
	late.Cancel()
}
