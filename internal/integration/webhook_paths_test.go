package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // the manifest under test verifies HMAC-SHA1
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/expr"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// probeManifest signs "<timestamp>.<body>" with HMAC-SHA1, base64 and a
// prefix, and has events whose match or payload fail at runtime.
const probeManifest = `version: 1
name: probe
triggers:
  verify: { kind: hmac-sha1, header: X-Sig, prefix: "sha1=", encoding: base64, message: "${{ headers['x-ts'] }}.${{ raw_body }}", credential: signing }
  timestamp: { header: X-Ts, format: unix_ms, max_skew: 1m }
  challenges:
    - match: "body.type == 'challenge'"
      body: "${{ body.nested.value }}"
  events:
    thing:
      match: "body.kind == 'thing'"
      payload: { name: "${{ body.name }}", tag: "${{ query.tag }}" }
    counted:
      match: "body.count > 3"
      payload: { count: "${{ body.count }}" }
    bad-payload:
      match: "body.kind == 'bad'"
      payload: { first: "${{ body.name.first }}" }
`

// sharedManifest verifies a shared secret header and an RFC 3339 timestamp.
const sharedManifest = `version: 1
name: shared
triggers:
  verify: { kind: shared-secret, header: X-Token }
  timestamp: { header: X-When, format: rfc3339 }
  events:
    any:
      match: "true"
      payload: { raw: "${{ raw_body }}" }
`

func (f *hookFixture) install(manifests map[string]string) {
	f.t.Helper()
	dir := filepath.Join(f.hub.Registry().Dir(), "test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	for name, src := range manifests {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(src), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.hub.Registry().Reload(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	if errs := f.hub.Registry().Snapshot().Errors(); len(errs) > 0 {
		f.t.Fatal(errs)
	}
}

func (f *hookFixture) manifest(name string) *Manifest {
	f.t.Helper()
	l, ok := f.hub.Registry().Snapshot().Get(name)
	if !ok {
		f.t.Fatalf("%s is not loaded", name)
	}
	return l.Manifest
}

func probeHeaders(secret string, ts time.Time, body []byte) map[string]string {
	tss := strconv.FormatInt(ts.UnixMilli(), 10)
	m := hmac.New(sha1.New, []byte(secret))
	m.Write([]byte(tss + "." + string(body)))
	return map[string]string{"X-Ts": tss, "X-Sig": "SHA1=" + base64.StdEncoding.EncodeToString(m.Sum(nil))}
}

func TestProbeWebhookPipeline(t *testing.T) {
	spec := &workflow.Spec{Version: 1, Name: "w", Triggers: []workflow.TriggerSpec{
		{Integration: "probe", Event: "thing"},
		{Integration: "probe", Event: "counted", Filter: "trigger.count.nope == 1"},
	}}
	f := newHookFixture(t, fakeSpecs{spec}, config.WebhooksConfig{})
	f.install(map[string]string{"probe": probeManifest})
	f.secret("probe.signing", "probe-secret-1234")

	body := []byte(`{"kind":"thing","name":"n1"}`)
	code, out, raw := f.post("/hooks/probe?tag=a&tag=b", body, probeHeaders("probe-secret-1234", f.now, body))
	if code != http.StatusAccepted || f.engine.count() != 1 {
		t.Fatalf("%d %s", code, raw)
	}
	if r := f.engine.last(); r.Trigger["name"] != "n1" || len(r.Trigger["tag"].([]any)) != 2 {
		t.Fatalf("repeated query values become an array: %v", r.Trigger)
	}
	_ = out

	// A payload that fails to evaluate is an event error on the delivery.
	body = []byte(`{"kind":"bad","name":"plain"}`)
	_, out, _ = f.post("/hooks/probe", body, probeHeaders("probe-secret-1234", f.now, body))
	if out["status"] != store.DeliveryNoMatch || !strings.Contains(out["error"].(string), "event bad-payload") {
		t.Fatalf("payload error: %v", out)
	}

	// A match that cannot be evaluated is reported, not treated as false.
	body = []byte(`{"count":"many"}`)
	_, out, _ = f.post("/hooks/probe", body, probeHeaders("probe-secret-1234", f.now, body))
	if !strings.Contains(out["error"].(string), "event counted: match:") {
		t.Fatalf("match error: %v", out)
	}

	// A filter that cannot be evaluated is reported on the workflow result.
	body = []byte(`{"count":5}`)
	_, out, _ = f.post("/hooks/probe", body, probeHeaders("probe-secret-1234", f.now, body))
	res := out["results"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(res["error"].(string), "filter:") || f.engine.count() != 1 {
		t.Fatalf("filter error: %v", out)
	}

	// A challenge whose body cannot be rendered is a 400.
	body = []byte(`{"type":"challenge"}`)
	code, out, _ = f.post("/hooks/probe", body, probeHeaders("probe-secret-1234", f.now, body))
	if code != http.StatusBadRequest || out["status"] != store.DeliveryError {
		t.Fatalf("challenge error: %d %v", code, out)
	}
}

func TestVerifyVariants(t *testing.T) {
	f := newHookFixture(t, nil, config.WebhooksConfig{})
	f.install(map[string]string{"probe": probeManifest, "shared": sharedManifest})
	f.secret("probe.signing", "probe-secret-1234")
	f.secret("shared.webhook_secret", "shared-token-1234")
	ctx := context.Background()
	probe, shared := f.manifest("probe"), f.manifest("shared")

	verify := func(m *Manifest, hdr map[string]string, body string) error {
		in := &inbound{integration: m.Name, headers: http.Header{}, body: []byte(body)}
		for k, v := range hdr {
			in.headers.Set(k, v)
		}
		return f.hub.verify(ctx, m, in, in.env())
	}
	good := probeHeaders("probe-secret-1234", f.now, []byte("{}"))
	if err := verify(probe, good, "{}"); err != nil {
		t.Fatalf("valid sha1/base64/prefix signature: %v", err)
	}
	when := f.now.UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		m    *Manifest
		hdr  map[string]string
		want string
	}{
		{"wrong prefix", probe, map[string]string{"X-Sig": "md5=abc", "X-Ts": good["X-Ts"]}, `signature must start with "sha1="`},
		{"short signature", probe, map[string]string{"X-Sig": "s", "X-Ts": good["X-Ts"]}, `signature must start with "sha1="`},
		{"bad base64", probe, map[string]string{"X-Sig": "sha1=***", "X-Ts": good["X-Ts"]}, "signature does not match"},
		{"message needs a missing header", probe, map[string]string{"X-Sig": good["X-Sig"]}, "render signed message"},
		{"stale unix_ms timestamp", probe, probeHeaders("probe-secret-1234", f.now.Add(-2*time.Minute), []byte("{}")), "possible replay"},
		{"shared secret mismatch", shared, map[string]string{"X-Token": "nope", "X-When": when}, "shared secret does not match"},
		{"missing timestamp", shared, map[string]string{"X-Token": "shared-token-1234"}, "missing X-When header"},
		{"invalid rfc3339", shared, map[string]string{"X-Token": "shared-token-1234", "X-When": "yesterday"}, "invalid timestamp"},
	}
	for _, c := range cases {
		err := verify(c.m, c.hdr, "{}")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
	if err := verify(shared, map[string]string{"X-Token": "shared-token-1234", "X-When": when}, "{}"); err != nil {
		t.Fatalf("shared secret + rfc3339: %v", err)
	}
	// The generic webhook accepts only its tokenized URL.
	if err := verify(f.manifest("webhook"), nil, "{}"); err == nil || !strings.Contains(err.Error(), "/hooks/webhook/<token>") {
		t.Fatal(err)
	}
	// The shared secret is masked in the stored headers: it is the secret.
	if hj := headersJSON(http.Header{"X-Token": {"shared-token-1234"}}, shared); strings.Contains(hj, "shared-token-1234") {
		t.Fatal(hj)
	}
}

func TestCheckTimestampFormats(t *testing.T) {
	f := newHookFixture(t, nil, config.WebhooksConfig{MaxSkew: time.Minute})
	now := f.now
	cases := []struct {
		ts   Timestamp
		raw  string
		want string // "" = accepted
	}{
		{Timestamp{Header: "T"}, strconv.FormatInt(now.Unix(), 10), ""},
		{Timestamp{Header: "T"}, strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10), "possible replay"},
		{Timestamp{Header: "T", MaxSkew: "5m"}, strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10), ""},
		{Timestamp{Header: "T"}, "12:00", "invalid timestamp"},
		{Timestamp{Header: "T", Format: "unix_ms"}, strconv.FormatInt(now.UnixMilli(), 10), ""},
		{Timestamp{Header: "T", Format: "rfc3339"}, now.Add(-30 * time.Second).Format(time.RFC3339), ""},
		{Timestamp{Header: "T", Format: "rfc3339"}, "", "missing T header"},
	}
	for _, c := range cases {
		err := f.hub.checkTimestamp(&c.ts, c.raw)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v %q: want %q, got %v", c.ts, c.raw, c.want, err)
		}
	}
}

func TestParseBodyAndConditions(t *testing.T) {
	form := parseBody("application/x-www-form-urlencoded; charset=utf-8", []byte("a=1&b=2&b=3"))
	m, ok := form.(map[string]any)
	if !ok || m["a"] != "1" || len(m["b"].([]any)) != 2 {
		t.Fatalf("form bodies become objects: %#v", form)
	}
	if v := parseBody("application/x-www-form-urlencoded", []byte("%zz")); v != "%zz" {
		t.Fatalf("an unparseable form body stays raw: %#v", v)
	}
	if v := parseBody("text/plain", []byte("not json")); v != "not json" {
		t.Fatalf("%#v", v)
	}
	if v := parseBody("", nil); v != "" {
		t.Fatalf("%#v", v)
	}
	if _, err := condTrue("body ==", expr.Env{}); err == nil {
		t.Fatal("a condition that does not parse is an error")
	}
	if ok, err := condTrue("body.missing == 1", expr.Env{"body": map[string]any{}}); ok || err != nil {
		t.Fatal("a missing field makes a condition false, not an error")
	}
	if _, err := Normalize(Event{Payload: map[string]string{"x": "${{ body ==  }}"}}, expr.Env{}); err == nil {
		t.Fatal("a payload template that does not parse is an error")
	}
}

type failingSpecSource struct{}

func (failingSpecSource) ActiveSpecs(context.Context) ([]*workflow.Spec, error) {
	return nil, errors.New("db locked")
}

func TestDispatchErrors(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{})
	ctx := context.Background()
	in := &inbound{integration: "github"}
	events := []EventResult{{Event: "pull_request.opened", Payload: map[string]any{"draft": false}}}

	f.hub.specs.Store(nil)
	if _, err := f.hub.dispatch(ctx, in, "d", "k", workflow.RunTriggerIntegration, events); err == nil || !strings.Contains(err.Error(), "no workflow source") {
		t.Fatal(err)
	}
	f.hub.SetSpecSource(failingSpecSource{})
	if _, err := f.hub.dispatch(ctx, in, "d", "k", workflow.RunTriggerIntegration, events); err == nil || !strings.Contains(err.Error(), "list workflows: db locked") {
		t.Fatal(err)
	}
	other := &workflow.Spec{Version: 1, Name: "other", Triggers: reviewSpec.Triggers}
	f.hub.SetSpecSource(fakeSpecs{reviewSpec, other})
	// A url-token delivery targets one workflow only.
	res, err := f.hub.dispatch(ctx, &inbound{integration: "github", workflow: "other"}, "d", "k", workflow.RunTriggerIntegration, events)
	if err != nil || len(res) != 1 || res[0].Workflow != "other" {
		t.Fatalf("%+v %v", res, err)
	}
	f.hub.enq.Store(nil)
	res, err = f.hub.dispatch(ctx, in, "d", "k", workflow.RunTriggerIntegration, events)
	if err == nil || len(res) != 2 || res[0].Error != "workflow engine is not wired" {
		t.Fatalf("%+v %v", res, err)
	}
}

// closingEngine closes the store while a delivery is in flight, so the
// outcome cannot be recorded; the webhook is still answered.
type closingEngine struct{ st *store.Store }

func (c closingEngine) EnqueueRun(context.Context, workflow.EnqueueRequest) (workflow.EnqueueResult, error) {
	_ = c.st.Close()
	return workflow.EnqueueResult{RunID: "r"}, nil
}

func TestDeliveryStoreFailures(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	body := fixture(t, "github_pull_request_opened.json")
	f.hub.SetEnqueuer(closingEngine{f.store})
	if code, out, _ := f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body)); code != http.StatusAccepted || out["status"] != store.DeliveryProcessed {
		t.Fatalf("an unrecorded outcome must not fail the webhook: %d %v", code, out)
	}
	// With the store gone the delivery cannot even be recorded.
	code, out, _ := f.post("/hooks/github", body, ghHeaders("pull_request", "d-2", "s3cret-hook", body))
	if code != http.StatusInternalServerError || out["error"] != "could not record delivery" {
		t.Fatalf("%d %v", code, out)
	}
	if _, err := f.hub.Redeliver(context.Background(), "x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a store error is not a not-found: %v", err)
	}
	if _, err := f.hub.Delivery(context.Background(), "x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a store error is not a not-found: %v", err)
	}
	if _, err := f.hub.Deliveries(context.Background(), "", "", 10); err == nil {
		t.Fatal("list error")
	}
}

func TestRedeliverErrors(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	ctx := context.Background()
	if _, err := f.hub.Redeliver(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.hub.Delivery(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	f.install(map[string]string{"shared": sharedManifest})
	f.secret("shared.webhook_secret", "shared-token-1234")
	_, out, raw := f.post("/hooks/shared", []byte("x"), map[string]string{"X-Token": "shared-token-1234", "X-When": f.now.UTC().Format(time.RFC3339)})
	id, _ := out["delivery_id"].(string)
	if out["status"] != store.DeliveryNoMatch || id == "" {
		t.Fatal(raw)
	}
	// The integration is removed after the delivery was recorded.
	if err := os.Remove(filepath.Join(f.hub.Registry().Dir(), "test", "shared.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Registry().Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Redeliver(ctx, id); err == nil || !strings.Contains(err.Error(), "no longer loaded") {
		t.Fatal(err)
	}
}

func TestInboundEnvQuery(t *testing.T) {
	in := &inbound{headers: http.Header{"X-A": {"1", "2"}, "X-Empty": {}}, query: url.Values{"one": {"1"}, "many": {"a", "b"}}}
	env := in.env()
	h := env["headers"].(map[string]any)
	q := env["query"].(map[string]any)
	if h["x-a"] != "1" || q["one"] != "1" || len(q["many"].([]any)) != 2 {
		t.Fatalf("%v %v", h, q)
	}
	if _, ok := h["x-empty"]; ok {
		t.Fatal("an empty header is not a field")
	}
}
