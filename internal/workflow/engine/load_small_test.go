//go:build !load

package engine

import "time"

// Sizes for the load test in the normal (and -race) suite.
const (
	loadRuns       = 40
	loadTimeout    = 60 * time.Second
	loadHeapBudget = 64 << 20
)
