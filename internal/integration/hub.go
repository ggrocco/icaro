package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v3"

	"icaro/internal/config"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// SecretStore is what the hub needs from the daemon secret store.
type SecretStore interface {
	CredentialStore
	Resolve(ctx context.Context, name string) (string, error)
}

// HubOptions configure a Hub.
type HubOptions struct {
	Dir      string // ~/.icaro/integrations
	Store    *store.Store
	Secrets  SecretStore
	Webhooks config.WebhooksConfig
	Settings func(integration string) config.IntegrationConfig
	Git      GitRunner
	Log      *slog.Logger
	Now      func() time.Time
	// PollInterval for the pack watcher (default 2s).
	PollInterval time.Duration
}

// Hub composes the registry, packs, webhook router and action executor.
type Hub struct {
	registry *Registry
	packs    *Packs
	store    *store.Store
	secrets  SecretStore
	executor *Executor
	log      *slog.Logger
	now      func() time.Time
	maxBody  int64
	maxSkew  time.Duration
	poll     time.Duration
	limiter  *limiter
	// retention and maxRejected bound the delivery records (webhooks.*).
	retention   time.Duration
	maxRejected int

	enq   atomic.Pointer[workflow.Enqueuer]
	specs atomic.Pointer[workflow.SpecSource]
}

// NewHub builds a hub and loads the packs.
func NewHub(o HubOptions) *Hub {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Settings == nil {
		o.Settings = func(string) config.IntegrationConfig { return config.IntegrationConfig{} }
	}
	maxBody := o.Webhooks.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	skew := o.Webhooks.MaxSkew
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	rate := o.Webhooks.RatePerMin
	if rate <= 0 {
		rate = 120
	}
	retention := o.Webhooks.Retention
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	maxRejected := o.Webhooks.MaxRejected
	if maxRejected <= 0 {
		maxRejected = 1000
	}
	reg := NewRegistry(o.Dir, o.Git)
	h := &Hub{
		registry: reg, packs: NewPacks(o.Dir, o.Git), store: o.Store, secrets: o.Secrets, log: o.Log, now: o.Now,
		maxBody: maxBody, maxSkew: skew, poll: o.PollInterval, limiter: newLimiter(rate, o.Now),
		retention: retention, maxRejected: maxRejected,
		executor: &Executor{Registry: reg, Secrets: o.Secrets, Settings: o.Settings},
	}
	if o.Store != nil {
		h.SetSpecSource(StoreSpecs{Store: o.Store})
	}
	return h
}

// Registry returns the manifest registry.
func (h *Hub) Registry() *Registry { return h.registry }

// Packs returns the pack manager.
func (h *Hub) Packs() *Packs { return h.packs }

// Executor returns the `uses:` action executor (workflow.ActionExecutor).
func (h *Hub) Executor() *Executor { return h.executor }

// Catalog returns a workflow.Catalog over the live registry.
func (h *Hub) Catalog() workflow.Catalog { return Catalog{R: h.registry} }

// SetEnqueuer wires the workflow engine.
func (h *Hub) SetEnqueuer(e workflow.Enqueuer) { h.enq.Store(&e) }

// SetSpecSource wires the source of active workflows (defaults to the
// store's workflows table).
func (h *Hub) SetSpecSource(s workflow.SpecSource) { h.specs.Store(&s) }

func (h *Hub) enqueuer() workflow.Enqueuer {
	if p := h.enq.Load(); p != nil {
		return *p
	}
	return nil
}

func (h *Hub) specSource() workflow.SpecSource {
	if p := h.specs.Load(); p != nil {
		return *p
	}
	return nil
}

// Name implements app.Component.
func (h *Hub) Name() string { return "integrations" }

// pruneEvery is how often Run prunes delivery records.
const pruneEvery = time.Hour

// Run watches packs, keeps the generic webhook tokens in sync, prunes
// delivery records and owns the rate limiter until ctx ends.
func (h *Hub) Run(ctx context.Context) error {
	go h.limiter.run(ctx)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		var pruned time.Time
		for {
			if _, err := h.SyncWebhookTokens(ctx); err != nil && ctx.Err() == nil {
				h.log.Warn("sync webhook tokens", "err", err)
			}
			if now := h.now(); now.Sub(pruned) >= pruneEvery {
				pruned = now
				if _, err := h.PruneDeliveries(ctx); err != nil && ctx.Err() == nil {
					h.log.Warn("prune webhook deliveries", "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return h.registry.Run(ctx, h.poll)
}

// PruneDeliveries applies webhooks.retention and webhooks.max_rejected
// to the delivery records. The retention is also the window in which a
// replay is recognized as a duplicate.
func (h *Hub) PruneDeliveries(ctx context.Context) (int64, error) {
	n, err := h.store.PruneDeliveries(ctx, h.now().Add(-h.retention), h.maxRejected)
	if n > 0 {
		h.log.Info("pruned webhook deliveries", "count", n)
	}
	return n, err
}

// SyncWebhookTokens ensures every active workflow subscribed to the
// generic `webhook` integration has a URL token, and returns all tokens.
func (h *Hub) SyncWebhookTokens(ctx context.Context) ([]*store.WebhookToken, error) {
	if src := h.specSource(); src != nil {
		specs, err := src.ActiveSpecs(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range specs {
			for _, tr := range s.Triggers {
				if tr.Integration == "webhook" {
					if _, err := h.store.EnsureWebhookToken(ctx, s.Name, newURLToken); err != nil {
						return nil, err
					}
					break
				}
			}
		}
	}
	return h.store.ListWebhookTokens(ctx)
}

// RotateWebhookToken issues a new URL token for a workflow.
func (h *Hub) RotateWebhookToken(ctx context.Context, wf string) (*store.WebhookToken, error) {
	if err := h.store.RotateWebhookToken(ctx, wf, newURLToken()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return h.store.EnsureWebhookToken(ctx, wf, newURLToken)
}

// ServeHook handles POST /hooks/{integration}.
func (h *Hub) ServeHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("integration")
	l, ok := h.registry.Snapshot().Get(name)
	if !ok || l.Manifest.Triggers == nil {
		writeHookJSON(w, http.StatusNotFound, map[string]string{"error": "no integration with triggers named " + name})
		return
	}
	if l.Manifest.Triggers.Verify.Kind == VerifyURLToken {
		writeHookJSON(w, http.StatusNotFound, map[string]string{"error": "post to /hooks/" + name + "/<token>"})
		return
	}
	h.serve(w, r, l.Manifest, "")
}

// ServeHookToken handles POST /hooks/webhook/{token}.
func (h *Hub) ServeHookToken(w http.ResponseWriter, r *http.Request) {
	l, ok := h.registry.Snapshot().Get("webhook")
	if !ok || l.Manifest.Triggers == nil {
		writeHookJSON(w, http.StatusNotFound, map[string]string{"error": "generic webhook integration is not loaded"})
		return
	}
	tok, err := h.store.WebhookTokenByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		// Rate-limit misses too, so tokens cannot be brute-forced quickly.
		h.limiter.allow("webhook:miss")
		writeHookJSON(w, http.StatusNotFound, map[string]string{"error": "unknown webhook token"})
		return
	}
	h.serve(w, r, l.Manifest, tok.Workflow)
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request, m *Manifest, wf string) {
	if !h.limiter.allow(m.Name) {
		w.Header().Set("Retry-After", "60")
		writeHookJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeHookJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body exceeds the webhook size limit"})
		return
	}
	in := &inbound{integration: m.Name, workflow: wf, method: r.Method, headers: r.Header.Clone(), query: r.URL.Query(), body: body}
	out, code := h.process(r.Context(), m, in)
	if out.challenge != "" || out.Status == store.DeliveryChallenge {
		ct := out.contentType
		if ct == "" {
			ct = "text/plain; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out.challenge)
		return
	}
	writeHookJSON(w, code, out)
}

func writeHookJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// DeliveryInfo is the API view of a delivery record.
type DeliveryInfo struct {
	ID          string              `json:"id"`
	Integration string              `json:"integration"`
	DeliveryKey string              `json:"delivery_key"`
	Workflow    string              `json:"workflow,omitempty"`
	Method      string              `json:"method"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Query       string              `json:"query,omitempty"`
	Body        string              `json:"body,omitempty"`
	BodySize    int64               `json:"body_size"`
	BodySHA256  string              `json:"body_sha256,omitempty"`
	// BodyTruncated marks a body stored only as a prefix (rejected).
	BodyTruncated bool             `json:"body_truncated,omitempty"`
	Verified      bool             `json:"verified"`
	VerifyError   string           `json:"verify_error,omitempty"`
	Status        string           `json:"status"`
	Error         string           `json:"error,omitempty"`
	Events        []EventResult    `json:"events"`
	Results       []WorkflowResult `json:"results"`
	RedeliveryOf  string           `json:"redelivery_of,omitempty"`
	ReceivedAt    string           `json:"received_at"`
}

func deliveryInfo(d *store.Delivery, full bool) DeliveryInfo {
	info := DeliveryInfo{
		ID: d.ID, Integration: d.Integration, DeliveryKey: d.DeliveryKey, Workflow: d.Workflow, Method: d.Method,
		Verified: d.Verified, VerifyError: d.VerifyError, Status: d.Status, Error: d.Error, RedeliveryOf: d.RedeliveryOf,
		ReceivedAt: d.ReceivedAt.UTC().Format(time.RFC3339), Events: []EventResult{}, Results: []WorkflowResult{},
		BodySize: d.BodySize, BodySHA256: d.BodySHA256, BodyTruncated: int64(len(d.Body)) < d.BodySize,
	}
	_ = json.Unmarshal([]byte(d.EventsJSON), &info.Events)
	_ = json.Unmarshal([]byte(d.ResultsJSON), &info.Results)
	if full {
		_ = json.Unmarshal([]byte(d.HeadersJSON), &info.Headers)
		info.Query, info.Body = d.Query, string(d.Body)
	}
	return info
}

// Deliveries lists recent deliveries (no bodies).
func (h *Hub) Deliveries(ctx context.Context, integration, status string, limit int) ([]DeliveryInfo, error) {
	ds, err := h.store.ListDeliveries(ctx, store.DeliveryFilter{Integration: integration, Status: status, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]DeliveryInfo, 0, len(ds))
	for _, d := range ds {
		out = append(out, deliveryInfo(d, false))
	}
	return out, nil
}

// Delivery returns one delivery with headers and body.
func (h *Hub) Delivery(ctx context.Context, id string) (*DeliveryInfo, error) {
	d, err := h.store.GetDelivery(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	info := deliveryInfo(d, true)
	return &info, nil
}

// IntegrationInfo is the API view of a loaded integration.
type IntegrationInfo struct {
	IntegrationSummary
	Pack        string                         `json:"pack"`
	File        string                         `json:"file"`
	Shadows     bool                           `json:"shadows_builtin,omitempty"`
	EventsFull  map[string]workflow.EventInfo  `json:"event_details,omitempty"`
	ActionsFull map[string]workflow.ActionInfo `json:"action_details,omitempty"`
	Manifest    string                         `json:"manifest,omitempty"`
}

// Integrations lists loaded integrations.
func (h *Hub) Integrations() []IntegrationInfo {
	snap := h.registry.Snapshot()
	var out []IntegrationInfo
	for _, n := range snap.Names() {
		out = append(out, h.info(snap, n, false))
	}
	return out
}

// Integration returns one integration with its manifest and typed details.
func (h *Hub) Integration(name string) (*IntegrationInfo, error) {
	snap := h.registry.Snapshot()
	if _, ok := snap.Get(name); !ok {
		return nil, ErrNotFound
	}
	info := h.info(snap, name, true)
	return &info, nil
}

func (h *Hub) info(snap *Snapshot, name string, full bool) IntegrationInfo {
	l, _ := snap.Get(name)
	info := IntegrationInfo{IntegrationSummary: Summarize(l.Manifest), Pack: l.Pack, File: l.File, Shadows: l.Shadows}
	if full {
		info.Manifest = l.Source
		info.EventsFull = map[string]workflow.EventInfo{}
		for _, e := range snap.EventNames(name) {
			info.EventsFull[e], _ = snap.Event(name, e)
		}
		info.ActionsFull = map[string]workflow.ActionInfo{}
		for a := range l.Manifest.Actions {
			info.ActionsFull[a], _ = snap.Action(name + "." + a)
		}
	}
	return info
}

// StoreSpecs reads active workflows from the store's workflows table and
// decodes their YAML as workflow v1 specs. It is the default SpecSource;
// the workflow service may replace it.
type StoreSpecs struct{ Store *store.Store }

// ActiveSpecs implements workflow.SpecSource.
func (s StoreSpecs) ActiveSpecs(ctx context.Context) ([]*workflow.Spec, error) {
	ws, err := s.Store.ListWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	var out []*workflow.Spec
	for _, w := range ws {
		if w.Status != store.WorkflowActive {
			continue
		}
		var spec workflow.Spec
		if err := yaml.Unmarshal([]byte(w.SpecYAML), &spec); err != nil || spec.Version != 1 {
			continue
		}
		if spec.Name == "" {
			spec.Name = w.Name
		}
		out = append(out, &spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// limiter is a token bucket per key, owned by one goroutine. Until run
// starts (or after it stops) every request is allowed.
type limiter struct {
	perMin  float64
	now     func() time.Time
	req     chan limitReq
	running atomic.Bool
}

type limitReq struct {
	key   string
	reply chan bool
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(perMin int, now func() time.Time) *limiter {
	return &limiter{perMin: float64(perMin), now: now, req: make(chan limitReq)}
}

func (l *limiter) run(ctx context.Context) {
	l.running.Store(true)
	defer l.running.Store(false)
	buckets := map[string]*bucket{}
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-l.req:
			now := l.now()
			b, ok := buckets[r.key]
			if !ok {
				b = &bucket{tokens: l.perMin, at: now}
				buckets[r.key] = b
			}
			b.tokens = min(l.perMin, b.tokens+now.Sub(b.at).Minutes()*l.perMin)
			b.at = now
			if b.tokens >= 1 {
				b.tokens--
				r.reply <- true
			} else {
				r.reply <- false
			}
		}
	}
}

// allow reports whether a request for key may proceed.
func (l *limiter) allow(key string) bool {
	if !l.running.Load() {
		return true // not started (tests, CLI): fail open rather than drop webhooks
	}
	reply := make(chan bool, 1)
	select {
	case l.req <- limitReq{key: key, reply: reply}:
		return <-reply
	case <-time.After(time.Second):
		return true
	}
}
