package api

import (
	"bufio"
	"context"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"icaro/internal/store"
	"icaro/internal/workflow/events"
)

// TestLoadSSEFanOut opens sseSubscribers streams spread over sseRuns
// runs, finishes every run, and checks each stream gets exactly one
// snapshot and one terminal "end" before the server closes it, and that
// no handler goroutine outlives its stream (Phase 11 load test; -tags load
// runs the full size).
func TestLoadSSEFanOut(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.ApplyWorkflow(ctx, wfYAML, "test"); err != nil {
		t.Fatal(err)
	}
	runIDs := make([]string, sseRuns)
	for i := range runIDs {
		r, err := e.svc.StartRun(ctx, "hello", map[string]any{"who": "load"}, "test")
		if err != nil {
			t.Fatal(err)
		}
		runIDs[i] = r.ID
	}
	runtime.GC()
	baseG := runtime.NumGoroutine()

	type result struct {
		snapshots, ends, other int
		err                    error
	}
	results := make([]result, sseSubscribers)
	var ready, done sync.WaitGroup
	ready.Add(sseSubscribers)
	done.Add(sseSubscribers)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: sseSubscribers}}
	start := time.Now()
	for i := 0; i < sseSubscribers; i++ {
		go func(i int) {
			defer done.Done()
			signaled := false
			signal := func() {
				if !signaled {
					signaled = true
					ready.Done()
				}
			}
			defer signal()
			req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/runs/"+runIDs[i%len(runIDs)]+"/events", nil)
			req.Header.Set("Authorization", "Bearer "+e.reader.Token)
			resp, err := client.Do(req)
			if err != nil {
				results[i].err = err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				name, ok := strings.CutPrefix(sc.Text(), "event: ")
				if !ok {
					continue
				}
				switch name {
				case "snapshot":
					results[i].snapshots++
					signal() // subscribed: the handler subscribes before the snapshot
				case "end":
					results[i].ends++
				default:
					results[i].other++
				}
			}
			// Reaching EOF means the server closed the stream after "end".
			results[i].err = sc.Err()
		}(i)
	}
	ready.Wait()
	connected := time.Since(start)

	finish := time.Now()
	for _, id := range runIDs {
		if err := e.svc.Store().FinishRun(ctx, id, store.RunSucceeded, ""); err != nil {
			t.Fatal(err)
		}
		e.bus.Publish(events.Event{Type: events.TypeRun, RunID: id, Status: store.RunSucceeded})
	}
	waited := make(chan struct{})
	go func() { done.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(sseTimeout):
		t.Fatal("streams did not all end")
	}
	ended := time.Since(finish)
	for i, r := range results {
		if r.err != nil || r.snapshots != 1 || r.ends != 1 {
			t.Fatalf("stream %d: %+v (want one snapshot and exactly one end)", i, r)
		}
	}
	client.CloseIdleConnections()
	var g int
	for i := 0; i < 200; i++ {
		if g = runtime.NumGoroutine(); g <= baseG+5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if g > baseG+5 {
		t.Fatalf("goroutines %d -> %d after all streams ended", baseG, g)
	}
	t.Logf("sse: %d streams over %d runs connected in %s; all ended %s after the runs finished; goroutines %d -> %d",
		sseSubscribers, sseRuns, connected.Round(time.Millisecond), ended.Round(time.Millisecond), baseG, g)
}
