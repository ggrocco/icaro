package distill

import (
	"context"
	"fmt"
	"time"
)

// Source opens the model for a batch of distill jobs, as memory.distill
// configures it: off, an endpoint the user runs, or the managed CPU
// container with a pulled model.
type Source struct {
	Enabled bool
	// Endpoint and EndpointModel select a server the user runs.
	Endpoint, EndpointModel string
	// Model is a catalog name or "auto"; Tier is the machine's tier.
	Model, Tier string
	Store       Store
	Managed     Managed
}

// Skip reasons.
const (
	SkipDisabled = "the model stage is off (memory.distill.enabled: false)"
	SkipNoModel  = "no model is pulled (icaro memory model pull)"
)

// Open returns a completer and a function that releases it, or why the
// model stage is skipped.
func (s Source) Open(ctx context.Context) (Completer, func(), string, error) {
	if !s.Enabled {
		return nil, nil, SkipDisabled, nil
	}
	if s.Endpoint != "" {
		c := &Client{BaseURL: s.Endpoint, Model: s.EndpointModel}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := c.Ready(rctx); err != nil {
			return nil, nil, "", fmt.Errorf("memory.distill.endpoint %s: %w", s.Endpoint, err)
		}
		return c, func() {}, "", nil
	}
	m, ok, err := s.Store.Pick(s.Model, s.Tier)
	if err != nil {
		return nil, nil, "", err
	}
	if !ok {
		return nil, nil, SkipNoModel, nil
	}
	c, stop, err := s.Managed.Start(ctx, m)
	if err != nil {
		return nil, nil, "", err
	}
	return c, stop, "", nil
}
