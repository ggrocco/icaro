//go:build !load

package integration

import "time"

// Sizes for the await load test in the normal (and -race) suite.
const (
	awaitLoadSteps  = 100
	awaitLoadBudget = 5 * time.Second
)
