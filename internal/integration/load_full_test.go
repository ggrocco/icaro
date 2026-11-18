//go:build load

package integration

import "time"

// Full size: go test -tags load -run Load ./internal/integration
const (
	awaitLoadSteps  = 1000
	awaitLoadBudget = 500 * time.Millisecond
)
