package integration

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is a provider-defined verification kind
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"icaro/internal/expr"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// inbound is a webhook request reduced to what the pipeline needs.
type inbound struct {
	integration string
	workflow    string // url-token target
	method      string
	headers     http.Header
	query       url.Values
	body        []byte
}

// env builds the expression roots for trigger sections.
func (in *inbound) env() expr.Env {
	h := map[string]any{}
	for k, v := range in.headers {
		if len(v) > 0 {
			h[strings.ToLower(k)] = v[0]
		}
	}
	q := map[string]any{}
	for k, v := range in.query {
		if len(v) == 1 {
			q[k] = v[0]
		} else {
			vals := make([]any, len(v))
			for i, s := range v {
				vals[i] = s
			}
			q[k] = vals
		}
	}
	return expr.Env{"headers": h, "query": q, "raw_body": string(in.body), "body": parseBody(in.headers.Get("Content-Type"), in.body)}
}

func parseBody(contentType string, raw []byte) any {
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt == "application/x-www-form-urlencoded" {
		if vals, err := url.ParseQuery(string(raw)); err == nil {
			m := map[string]any{}
			for k, v := range vals {
				if len(v) == 1 {
					m[k] = v[0]
				} else {
					arr := make([]any, len(v))
					for i, s := range v {
						arr[i] = s
					}
					m[k] = arr
				}
			}
			return m
		}
	}
	var v any
	if len(raw) > 0 && json.Unmarshal(raw, &v) == nil {
		return v
	}
	return string(raw)
}

// verifyError is a verification failure.
type verifyError struct{ msg string }

func (e *verifyError) Error() string { return e.msg }

// verify authenticates an inbound request per the manifest.
func (h *Hub) verify(ctx context.Context, m *Manifest, in *inbound, env expr.Env) error {
	t := m.Triggers
	v := t.Verify
	if v.Kind == VerifyURLToken {
		if in.workflow == "" {
			return &verifyError{"this integration only accepts /hooks/webhook/<token>"}
		}
		return nil
	}
	cred := v.Credential
	if cred == "" {
		cred = "webhook_secret"
	}
	secretName := m.Name + "." + cred
	secret, err := h.secrets.Resolve(ctx, secretName)
	if err != nil {
		return &verifyError{fmt.Sprintf("missing webhook secret %s (add it with `icaro credential add %s %s`)", secretName, m.Name, cred)}
	}
	got := in.headers.Get(v.Header)
	if got == "" {
		return &verifyError{fmt.Sprintf("missing %s header", v.Header)}
	}
	switch v.Kind {
	case VerifySharedSecret:
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			return &verifyError{"shared secret does not match"}
		}
	case VerifyHMACSHA256, VerifyHMACSHA1:
		msg := string(in.body)
		if v.Message != "" {
			msg, err = expr.RenderString(v.Message, env)
			if err != nil {
				return &verifyError{"render signed message: " + err.Error()}
			}
		}
		var newHash func() hash.Hash = sha256.New
		if v.Kind == VerifyHMACSHA1 {
			newHash = sha1.New
		}
		mac := hmac.New(newHash, []byte(secret))
		mac.Write([]byte(msg))
		want := mac.Sum(nil)
		sig := got
		if v.Prefix != "" {
			if len(sig) < len(v.Prefix) || !strings.EqualFold(sig[:len(v.Prefix)], v.Prefix) {
				return &verifyError{fmt.Sprintf("signature must start with %q", v.Prefix)}
			}
			sig = sig[len(v.Prefix):]
		}
		var provided []byte
		if v.Encoding == "base64" {
			provided, err = base64.StdEncoding.DecodeString(sig)
		} else {
			provided, err = hex.DecodeString(strings.ToLower(sig))
		}
		if err != nil || !hmac.Equal(provided, want) {
			return &verifyError{"signature does not match"}
		}
	}
	if t.Timestamp != nil {
		if err := h.checkTimestamp(t.Timestamp, in.headers.Get(t.Timestamp.Header)); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) checkTimestamp(ts *Timestamp, raw string) error {
	if raw == "" {
		return &verifyError{fmt.Sprintf("missing %s header", ts.Header)}
	}
	var at time.Time
	switch ts.Format {
	case "rfc3339":
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return &verifyError{"invalid timestamp"}
		}
		at = t
	default:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return &verifyError{"invalid timestamp"}
		}
		if ts.Format == "unix_ms" {
			at = time.UnixMilli(int64(n))
		} else {
			at = time.Unix(int64(n), 0)
		}
	}
	skew := h.maxSkew
	if ts.MaxSkew != "" {
		if d, err := time.ParseDuration(ts.MaxSkew); err == nil {
			skew = d
		}
	}
	if d := h.now().Sub(at); d > skew || d < -skew {
		return &verifyError{fmt.Sprintf("timestamp is %s away from now (max %s): possible replay", d.Round(time.Second), skew)}
	}
	return nil
}

// deliveryKey is the provider's delivery id, else a stable hash of the
// signed body (retries of an identical signed request dedupe), else — for
// unsigned url-token requests — a fresh id.
func deliveryKey(m *Manifest, in *inbound, env expr.Env) string {
	if m.Triggers.DeliveryID != "" {
		if s, err := expr.RenderString(m.Triggers.DeliveryID, env); err == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if m.Triggers.Verify.Kind == VerifyURLToken {
		return "auto:" + store.NewID()
	}
	sum := sha256.Sum256(in.body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EventResult is one matched event.
type EventResult struct {
	Event   string         `json:"event"`
	Payload map[string]any `json:"payload,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// WorkflowResult is what happened for one subscribed workflow.
type WorkflowResult struct {
	Workflow     string `json:"workflow"`
	Event        string `json:"event"`
	Trigger      int    `json:"trigger"`
	Filtered     bool   `json:"filtered,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
	Skipped      string `json:"skipped,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Outcome summarizes a processed delivery.
type Outcome struct {
	DeliveryID string           `json:"delivery_id"`
	Status     string           `json:"status"`
	Error      string           `json:"error,omitempty"`
	Duplicate  string           `json:"duplicate_of,omitempty"`
	Events     []EventResult    `json:"events"`
	Results    []WorkflowResult `json:"results"`

	challenge   string
	contentType string
}

func condTrue(src string, env expr.Env) (bool, error) {
	c, err := expr.ParseCondition(src)
	if err != nil {
		return false, err
	}
	ok, err := c.EvalBool(env)
	var mk *expr.MissingKeyError
	if errors.As(err, &mk) {
		return false, nil
	}
	return ok, err
}

// match evaluates events and normalizes their payloads.
func matchEvents(m *Manifest, env expr.Env) []EventResult {
	var out []EventResult
	for _, name := range sortedKeys(m.Triggers.Events) {
		ev := m.Triggers.Events[name]
		ok, err := condTrue(ev.Match, env)
		if err != nil {
			out = append(out, EventResult{Event: name, Error: "match: " + err.Error()})
			continue
		}
		if !ok {
			continue
		}
		payload, err := Normalize(ev, env)
		if err != nil {
			out = append(out, EventResult{Event: name, Error: err.Error()})
			continue
		}
		out = append(out, EventResult{Event: name, Payload: payload})
	}
	return out
}

// Normalize maps a raw request onto an event's ${{ trigger.* }} payload.
func Normalize(ev Event, env expr.Env) (map[string]any, error) {
	payload := map[string]any{}
	for _, field := range sortedKeys(ev.Payload) {
		t, err := expr.ParseTemplate(ev.Payload[field])
		if err != nil {
			return nil, fmt.Errorf("payload %s: %w", field, err)
		}
		v, err := t.Eval(env)
		if err != nil {
			return nil, fmt.Errorf("payload %s: %w", field, err)
		}
		payload[field] = v
	}
	return payload, nil
}

// dispatch matches subscribed workflows, applies filters and enqueues.
func (h *Hub) dispatch(ctx context.Context, in *inbound, deliveryID, key, kind string, events []EventResult) ([]WorkflowResult, error) {
	src := h.specSource()
	if src == nil {
		return nil, errors.New("no workflow source is wired into the integration hub")
	}
	specs, err := src.ActiveSpecs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	enq := h.enqueuer()
	var results []WorkflowResult
	var firstErr error
	for _, ev := range events {
		if ev.Error != "" {
			continue
		}
		for _, spec := range specs {
			if in.workflow != "" && spec.Name != in.workflow {
				continue
			}
			for idx, tr := range spec.Triggers {
				if tr.Integration != in.integration || tr.Event != ev.Event {
					continue
				}
				res := WorkflowResult{Workflow: spec.Name, Event: ev.Event, Trigger: idx}
				if tr.Filter != "" {
					ok, err := condTrue(tr.Filter, expr.Env{"trigger": ev.Payload})
					if err != nil {
						res.Error = "filter: " + err.Error()
						results = append(results, res)
						continue
					}
					if !ok {
						res.Filtered, res.Skipped = true, "filter did not match"
						results = append(results, res)
						continue
					}
				}
				if enq == nil {
					res.Error = "workflow engine is not wired"
					firstErr = errors.New(res.Error)
					results = append(results, res)
					continue
				}
				ikey := fmt.Sprintf("%s:%s:%s", in.integration, key, spec.Name)
				if kind == workflow.RunTriggerRedeliver {
					ikey = fmt.Sprintf("redeliver:%s:%s", deliveryID, spec.Name)
				}
				r, err := enq.EnqueueRun(ctx, workflow.EnqueueRequest{
					Workflow: spec.Name, TriggerKind: kind, Trigger: ev.Payload, IdempotencyKey: ikey,
					Actor: "integration:" + in.integration, DeliveryID: deliveryID,
				})
				if err != nil {
					res.Error = err.Error()
					if firstErr == nil {
						firstErr = err
					}
				} else {
					res.RunID, res.Deduplicated, res.Skipped = r.RunID, r.Deduplicated, r.Skipped
				}
				results = append(results, res)
			}
		}
	}
	return results, firstErr
}

// process runs the pipeline for a fresh request: persist → verify →
// dedupe → challenge → match → normalize → filter → enqueue.
func (h *Hub) process(ctx context.Context, m *Manifest, in *inbound) (*Outcome, int) {
	env := in.env()
	vErr := h.verify(ctx, m, in, env)
	d := &store.Delivery{
		Integration: in.integration, Workflow: in.workflow, Method: in.method,
		HeadersJSON: headersJSON(in.headers, m), Query: in.query.Encode(), Body: in.body,
		Verified: vErr == nil, Status: store.DeliveryError,
	}
	if vErr == nil {
		d.DeliveryKey = deliveryKey(m, in, env)
	} else {
		d.DeliveryKey = "unverified:" + store.NewID()
		d.VerifyError, d.Status = vErr.Error(), store.DeliveryRejected
	}
	if err := h.store.CreateDelivery(ctx, d); err != nil {
		h.log.Error("persist delivery", "integration", in.integration, "err", err)
		return &Outcome{Status: store.DeliveryError, Error: "could not record delivery"}, http.StatusInternalServerError
	}
	out := &Outcome{DeliveryID: d.ID, Events: []EventResult{}, Results: []WorkflowResult{}}
	if vErr != nil {
		out.Status, out.Error = store.DeliveryRejected, vErr.Error()
		h.log.Warn("webhook rejected", "integration", in.integration, "delivery", d.ID, "reason", vErr)
		return out, http.StatusUnauthorized
	}
	if prev, err := h.store.FindProcessedDelivery(ctx, in.integration, d.DeliveryKey, in.workflow, d.ID); err == nil {
		out.Status, out.Duplicate = store.DeliveryDuplicate, prev.ID
		h.finish(ctx, d.ID, out)
		return out, http.StatusOK
	}
	for _, c := range m.Triggers.Challenges {
		if ok, _ := condTrue(c.Match, env); ok {
			body, err := expr.RenderString(c.Body, env)
			if err != nil {
				out.Status, out.Error = store.DeliveryError, err.Error()
				h.finish(ctx, d.ID, out)
				return out, http.StatusBadRequest
			}
			out.Status, out.challenge, out.contentType = store.DeliveryChallenge, body, c.ContentType
			h.finish(ctx, d.ID, out)
			return out, http.StatusOK
		}
	}
	h.run(ctx, d.ID, d.DeliveryKey, workflow.RunTriggerIntegration, m, in, env, out)
	code := http.StatusAccepted
	if out.Status == store.DeliveryError {
		code = http.StatusServiceUnavailable // providers retry; dedupe makes that safe
	}
	return out, code
}

func (h *Hub) run(ctx context.Context, id, key, kind string, m *Manifest, in *inbound, env expr.Env, out *Outcome) {
	out.Events = matchEvents(m, env)
	if out.Events == nil {
		out.Events = []EventResult{}
	}
	results, err := h.dispatch(ctx, in, id, key, kind, out.Events)
	if results != nil {
		out.Results = results
	}
	switch {
	case err != nil:
		out.Status, out.Error = store.DeliveryError, err.Error()
	case len(out.Results) == 0:
		out.Status = store.DeliveryNoMatch
	default:
		out.Status = store.DeliveryProcessed
	}
	for _, ev := range out.Events {
		if ev.Error != "" && out.Error == "" {
			out.Error = "event " + ev.Event + ": " + ev.Error
		}
	}
	h.finish(ctx, id, out)
	h.log.Info("webhook processed", "integration", in.integration, "delivery", id, "status", out.Status, "runs", len(out.Results))
}

func (h *Hub) finish(ctx context.Context, id string, out *Outcome) {
	ev, _ := json.Marshal(out.Events)
	res, _ := json.Marshal(out.Results)
	if err := h.store.FinishDelivery(context.WithoutCancel(ctx), id, out.Status, out.Error, string(ev), string(res)); err != nil {
		h.log.Error("record delivery outcome", "delivery", id, "err", err)
	}
}

// headersJSON serializes headers for the delivery record with credential
// headers masked (a shared-secret header *is* the secret).
func headersJSON(hdr http.Header, m *Manifest) string {
	masked := map[string]bool{"authorization": true, "cookie": true, "proxy-authorization": true}
	if m != nil && m.Triggers != nil && m.Triggers.Verify.Kind == VerifySharedSecret {
		masked[strings.ToLower(m.Triggers.Verify.Header)] = true
	}
	out := map[string][]string{}
	for k, v := range hdr {
		if masked[strings.ToLower(k)] {
			out[k] = []string{"***"}
			continue
		}
		out[k] = v
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// Redeliver re-runs the pipeline from a stored, verified delivery. The
// new record links to the original and gets a fresh idempotency key.
func (h *Hub) Redeliver(ctx context.Context, id string) (*Outcome, error) {
	orig, err := h.store.GetDelivery(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !orig.Verified {
		return nil, fmt.Errorf("delivery %s failed verification (%s) and cannot be redelivered", id, orig.VerifyError)
	}
	l, ok := h.registry.Snapshot().Get(orig.Integration)
	if !ok || l.Manifest.Triggers == nil {
		return nil, fmt.Errorf("integration %q is no longer loaded", orig.Integration)
	}
	in := &inbound{integration: orig.Integration, workflow: orig.Workflow, method: orig.Method, headers: http.Header{}, body: orig.Body}
	var hdr map[string][]string
	_ = json.Unmarshal([]byte(orig.HeadersJSON), &hdr)
	for k, v := range hdr {
		in.headers[k] = v
	}
	in.query, _ = url.ParseQuery(orig.Query)
	d := &store.Delivery{
		Integration: orig.Integration, DeliveryKey: orig.DeliveryKey, Workflow: orig.Workflow, Method: orig.Method,
		HeadersJSON: orig.HeadersJSON, Query: orig.Query, Body: orig.Body, Verified: true, Status: store.DeliveryError,
		RedeliveryOf: orig.ID,
	}
	if err := h.store.CreateDelivery(ctx, d); err != nil {
		return nil, err
	}
	out := &Outcome{DeliveryID: d.ID, Events: []EventResult{}, Results: []WorkflowResult{}}
	h.run(ctx, d.ID, d.DeliveryKey, workflow.RunTriggerRedeliver, l.Manifest, in, in.env(), out)
	return out, nil
}

// ErrNotFound reports an unknown delivery, integration or token.
var ErrNotFound = errors.New("not found")

// newURLToken returns a random URL-safe token.
func newURLToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
