package cli

import (
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2025, 3, 10, 15, 4, 0, 0, time.UTC)
	got, err := parseSince("7d", now)
	if err != nil || !got.Equal(time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v", got, err)
	}
	got, err = parseSince("2025-01-02", now)
	if err != nil || got.Day() != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := parseSince("yesterday", now); err == nil {
		t.Fatal("bad input accepted")
	}
}
