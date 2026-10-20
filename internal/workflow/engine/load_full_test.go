//go:build load

package engine

import "time"

// Full load test size: go test -tags load -run Load ./internal/workflow/engine
const (
	loadRuns       = 500
	loadTimeout    = 5 * time.Minute
	loadHeapBudget = 256 << 20
)
