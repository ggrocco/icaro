//go:build !load

package api

import "time"

// Sizes for the SSE load test in the normal (and -race) suite.
const (
	sseRuns        = 10
	sseSubscribers = 40
	sseTimeout     = 30 * time.Second
)
