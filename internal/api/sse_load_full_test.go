//go:build load

package api

import "time"

// Full SSE load size: go test -tags load -run LoadSSE ./internal/api
const (
	sseRuns        = 50
	sseSubscribers = 200
	sseTimeout     = 60 * time.Second
)
