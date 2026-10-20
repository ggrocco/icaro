// Package service implements the daemon's workflow use-cases on top of
// the store. The CLI, HTTP API, MCP server and UI are thin adapters over
// this package (architecture §3).
package service

import (
	"context"
	"log/slog"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

// RunnerControl is what the service needs from the workflow engine.
type RunnerControl interface {
	Wake()
	Cancel(runID string)
}

// Service bundles the use-cases.
type Service struct {
	store      *store.Store
	key        *crypto.Key
	runner     RunnerControl
	catalog    workflow.Catalog
	secrets    workflow.SecretResolver
	events     *events.Bus
	logsDir    string
	maxTimeout time.Duration
	log        *slog.Logger
}

// Options configure a Service. Runner, Catalog, Secrets and Events may be
// nil and attached later with the setters, during daemon wiring.
type Options struct {
	Store      *store.Store
	Key        *crypto.Key
	Runner     RunnerControl
	Catalog    workflow.Catalog
	Secrets    workflow.SecretResolver
	Events     *events.Bus
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
	return &Service{
		store: o.Store, key: o.Key, runner: o.Runner, catalog: o.Catalog, secrets: o.Secrets, events: o.Events,
		logsDir: o.LogsDir, maxTimeout: o.MaxTimeout, log: o.Log,
	}
}

// The setters attach collaborators built after the service; they are only
// called while the daemon is being wired, before it serves requests.

// SetRunner attaches the workflow engine.
func (s *Service) SetRunner(r RunnerControl) { s.runner = r }

// SetCatalog attaches the integration catalog used by validation.
func (s *Service) SetCatalog(c workflow.Catalog) { s.catalog = c }

// SetSecrets attaches the secret store used to check secret references.
func (s *Service) SetSecrets(r workflow.SecretResolver) { s.secrets = r }

// SetEvents attaches the run event bus.
func (s *Service) SetEvents(b *events.Bus) { s.events = b }

// Store exposes the underlying store for adapters that need raw access
// (health checks).
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

var (
	_ workflow.Enqueuer   = (*Service)(nil)
	_ workflow.SpecSource = (*Service)(nil)
)
