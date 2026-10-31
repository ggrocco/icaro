package serial

import (
	"sync"
	"testing"
)

func TestDoRunsOneAtATime(t *testing.T) {
	q := New()
	n := 0
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.Do(func() { n++ })
		}()
	}
	wg.Wait()
	var got int
	q.Do(func() { got = n })
	if got != 100 {
		t.Fatalf("got %d, want 100", got)
	}
}
