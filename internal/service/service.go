// Package service implements the engine's use-cases on top of the store.
// REST, CLI and MCP are thin adapters over this package.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/ggrocco/icaro/internal/crypto"
	"github.com/ggrocco/icaro/internal/store"
)

// RunnerControl is what the service needs from an in-process runner.
type RunnerControl interface {
	Wake()
	Cancel(runID string)
}

// Service bundles the use-cases.
type Service struct {
	store      *store.Store
	key        *crypto.Key
	runner     RunnerControl // may be nil (server-only role)
	logsDir    string
	maxTimeout time.Duration
	log        *slog.Logger
}

// Options configure a Service.
type Options struct {
	Store      *store.Store
	Key        *crypto.Key
	Runner     RunnerControl
	LogsDir    string
	MaxTimeout time.Duration
	Log        *slog.Logger
}

// New creates a Service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.MaxTimeout <= 0 {
		o.MaxTimeout = 30 * time.Minute
	}
	return &Service{store: o.Store, key: o.Key, runner: o.Runner, logsDir: o.LogsDir, maxTimeout: o.MaxTimeout, log: o.Log}
}

// Store exposes the underlying store for adapters that need raw access
// (migrations, health checks).
func (s *Service) Store() *store.Store { return s.store }

// Authenticate resolves a bearer token to its record.
func (s *Service) Authenticate(ctx context.Context, token string) (*store.APIToken, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}
	t, err := s.store.GetTokenByHash(ctx, crypto.HashToken(token))
	if err != nil {
		return nil, ErrUnauthorized
	}
	_ = s.store.TouchToken(ctx, t.ID)
	return t, nil
}

// ScopeAllows reports whether a token scope satisfies the required scope.
func ScopeAllows(have, need string) bool {
	rank := map[string]int{store.ScopeRead: 1, store.ScopeWrite: 2, store.ScopeAdmin: 3}
	return rank[have] >= rank[need] && rank[need] > 0
}
