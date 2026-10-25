package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/crypto"
	"icaro/internal/secrets"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// fakeEngine mimics the workflow service: it dedupes on the idempotency key.
type fakeEngine struct {
	mu   sync.Mutex
	reqs []workflow.EnqueueRequest
	keys map[string]string
	fail error
}

func (f *fakeEngine) EnqueueRun(_ context.Context, r workflow.EnqueueRequest) (workflow.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return workflow.EnqueueResult{}, f.fail
	}
	if f.keys == nil {
		f.keys = map[string]string{}
	}
	if id, ok := f.keys[r.IdempotencyKey]; ok {
		return workflow.EnqueueResult{RunID: id, Deduplicated: true}, nil
	}
	f.reqs = append(f.reqs, r)
	id := fmt.Sprintf("run-%d", len(f.reqs))
	f.keys[r.IdempotencyKey] = id
	return workflow.EnqueueResult{RunID: id}, nil
}

func (f *fakeEngine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeEngine) last() workflow.EnqueueRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

type fakeSpecs []*workflow.Spec

func (f fakeSpecs) ActiveSpecs(context.Context) ([]*workflow.Spec, error) { return f, nil }

type hookFixture struct {
	t       *testing.T
	hub     *Hub
	srv     *httptest.Server
	engine  *fakeEngine
	secrets *secrets.Store
	store   *store.Store
	now     time.Time
}

func newHookFixture(t *testing.T, specs fakeSpecs, wh config.WebhooksConfig) *hookFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	sec := secrets.New(db, key)
	f := &hookFixture{t: t, engine: &fakeEngine{}, secrets: sec, store: db, now: time.Unix(1760954400, 0)}
	f.hub = NewHub(HubOptions{Dir: t.TempDir(), Store: db, Secrets: sec, Webhooks: wh, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return f.now }})
	f.hub.SetEnqueuer(f.engine)
	f.hub.SetSpecSource(specs)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{integration}", f.hub.ServeHook)
	mux.HandleFunc("POST /hooks/webhook/{token}", f.hub.ServeHookToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *hookFixture) secret(name, v string) {
	if err := f.secrets.Set(context.Background(), name, v, nil); err != nil {
		f.t.Fatal(err)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

func sign(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func (f *hookFixture) post(path string, body []byte, hdr map[string]string) (int, map[string]any, string) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func ghHeaders(event, delivery, secret string, body []byte) map[string]string {
	return map[string]string{"X-GitHub-Event": event, "X-GitHub-Delivery": delivery, "X-Hub-Signature-256": "sha256=" + sign(secret, string(body))}
}

var reviewSpec = &workflow.Spec{Version: 1, Name: "review", Triggers: []workflow.TriggerSpec{
	{Type: "manual"},
	{Integration: "github", Event: "pull_request.opened", Filter: "trigger.draft == false"},
}}

func TestGitHubWebhookEnqueuesOnceAndRedelivers(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	body := fixture(t, "github_pull_request_opened.json")

	code, out, raw := f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if code != http.StatusAccepted || out["status"] != store.DeliveryProcessed {
		t.Fatalf("%d %s", code, raw)
	}
	if f.engine.count() != 1 {
		t.Fatal("want one run")
	}
	req := f.engine.last()
	if req.Workflow != "review" || req.TriggerKind != workflow.RunTriggerIntegration || req.IdempotencyKey != "github:d-1:review" {
		t.Fatalf("%+v", req)
	}
	// A step can consume ${{ trigger.pr_number }}.
	if req.Trigger["pr_number"] != float64(42) || req.Trigger["repo"] != "acme/app" || req.Trigger["head_ref"] != "fix-flux" || req.Trigger["draft"] != false {
		t.Fatalf("payload %v", req.Trigger)
	}
	firstID := out["delivery_id"].(string)

	// The provider retries the same delivery: recorded, not re-run.
	code, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if code != http.StatusOK || out["status"] != store.DeliveryDuplicate || out["duplicate_of"] != firstID || f.engine.count() != 1 {
		t.Fatalf("duplicate: %d %v (runs %d)", code, out, f.engine.count())
	}

	// Any recorded delivery can be redelivered.
	res, err := f.hub.Redeliver(context.Background(), firstID)
	if err != nil || res.Status != store.DeliveryProcessed || f.engine.count() != 2 {
		t.Fatalf("redeliver: %+v %v", res, err)
	}
	if r := f.engine.last(); r.TriggerKind != workflow.RunTriggerRedeliver || !strings.HasPrefix(r.IdempotencyKey, "redeliver:") || r.Trigger["pr_number"] != float64(42) {
		t.Fatalf("%+v", r)
	}
	info, err := f.hub.Delivery(context.Background(), res.DeliveryID)
	if err != nil || info.RedeliveryOf != firstID || len(info.Results) != 1 || info.Results[0].RunID != "run-2" {
		t.Fatalf("%+v %v", info, err)
	}
	list, _ := f.hub.Deliveries(context.Background(), "github", "", 10)
	if len(list) != 3 {
		t.Fatalf("deliveries: %d", len(list))
	}
}

func TestWebhookRejectsBadSignatureAndMissingSecret(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{})
	body := fixture(t, "github_pull_request_opened.json")
	code, out, _ := f.post("/hooks/github", body, ghHeaders("pull_request", "d-2", "whatever", body))
	if code != http.StatusUnauthorized || !strings.Contains(out["error"].(string), "missing webhook secret github.webhook_secret") {
		t.Fatalf("%d %v", code, out)
	}
	f.secret("github.webhook_secret", "s3cret-hook")
	code, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-2", "wrong-secret", body))
	if code != http.StatusUnauthorized || out["error"] != "signature does not match" || f.engine.count() != 0 {
		t.Fatalf("%d %v", code, out)
	}
	rejected, _ := f.hub.Deliveries(context.Background(), "", store.DeliveryRejected, 10)
	if len(rejected) != 2 || rejected[0].Verified {
		t.Fatalf("rejected deliveries must be recorded: %+v", rejected)
	}
	if _, err := f.hub.Redeliver(context.Background(), rejected[0].ID); err == nil {
		t.Fatal("unverified deliveries cannot be redelivered")
	}
	code, _, _ = f.post("/hooks/nope", body, nil)
	if code != http.StatusNotFound {
		t.Fatal(code)
	}
}

func TestWebhookFilterSkipsBeforeEnqueue(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	body := bytes.Replace(fixture(t, "github_pull_request_opened.json"), []byte(`"draft":false`), []byte(`"draft":true`), 1)
	code, out, raw := f.post("/hooks/github", body, ghHeaders("pull_request", "d-3", "s3cret-hook", body))
	if code != http.StatusAccepted || f.engine.count() != 0 {
		t.Fatalf("%d %s", code, raw)
	}
	results := out["results"].([]any)
	if r := results[0].(map[string]any); r["filtered"] != true {
		t.Fatalf("%v", r)
	}
	// An event nobody subscribes to is no_match.
	push := fixture(t, "github_push.json")
	_, out, _ = f.post("/hooks/github", push, ghHeaders("push", "d-4", "s3cret-hook", push))
	if out["status"] != store.DeliveryNoMatch {
		t.Fatal(out)
	}
	// A failing engine yields 503 so the provider retries; the retry is not a duplicate.
	f.engine.fail = fmt.Errorf("db down")
	body = fixture(t, "github_pull_request_opened.json")
	code, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-5", "s3cret-hook", body))
	if code != http.StatusServiceUnavailable || out["status"] != store.DeliveryError {
		t.Fatal(code, out)
	}
	f.engine.fail = nil
	code, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-5", "s3cret-hook", body))
	if code != http.StatusAccepted || out["status"] != store.DeliveryProcessed || f.engine.count() != 1 {
		t.Fatal(code, out)
	}
}

func TestSlackChallengeAndReplayProtection(t *testing.T) {
	spec := &workflow.Spec{Version: 1, Name: "deploy-bot", Triggers: []workflow.TriggerSpec{{Integration: "slack", Event: "app_mention"}}}
	f := newHookFixture(t, fakeSpecs{spec}, config.WebhooksConfig{})
	f.secret("slack.webhook_secret", "8f742231b10e8888abcd99yyyzzz85a5")
	slackHdr := func(ts int64, body []byte) map[string]string {
		tss := strconv.FormatInt(ts, 10)
		return map[string]string{"X-Slack-Request-Timestamp": tss, "X-Slack-Signature": "v0=" + sign("8f742231b10e8888abcd99yyyzzz85a5", "v0:"+tss+":"+string(body))}
	}
	ch := fixture(t, "slack_url_verification.json")
	code, _, raw := f.post("/hooks/slack", ch, slackHdr(f.now.Unix(), ch))
	if code != 200 || raw != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" || f.engine.count() != 0 {
		t.Fatalf("challenge: %d %q", code, raw)
	}
	ev := fixture(t, "slack_app_mention.json")
	code, out, _ := f.post("/hooks/slack", ev, slackHdr(f.now.Add(-10*time.Minute).Unix(), ev))
	if code != http.StatusUnauthorized || !strings.Contains(out["error"].(string), "possible replay") {
		t.Fatalf("replay: %d %v", code, out)
	}
	code, _, _ = f.post("/hooks/slack", ev, slackHdr(f.now.Unix(), ev))
	if code != http.StatusAccepted || f.engine.count() != 1 {
		t.Fatal(code)
	}
	if r := f.engine.last(); r.Trigger["text"] != "<@U0LAN0Z89> deploy please" || r.Trigger["channel"] != "C123ABC456" || r.IdempotencyKey != "slack:Ev123ABC456:deploy-bot" {
		t.Fatalf("%+v", r)
	}
}

func TestEveryBuiltinNormalizesItsFixture(t *testing.T) {
	type tc struct {
		integration, fixture, event string
		headers                     func(secret string, body []byte) map[string]string
		check                       map[string]any
	}
	hmacHeader := func(name, prefix string) func(string, []byte) map[string]string {
		return func(s string, b []byte) map[string]string {
			return map[string]string{name: prefix + sign(s, string(b))}
		}
	}
	cases := []tc{
		{"github", "github_push.json", "push", func(s string, b []byte) map[string]string { return ghHeaders("push", "p1", s, b) },
			map[string]any{"ref": "refs/heads/main", "commit_count": float64(1), "pusher": "octocat"}},
		{"github", "github_issue_comment.json", "issue_comment.created", func(s string, b []byte) map[string]string { return ghHeaders("issue_comment", "c1", s, b) },
			map[string]any{"issue_number": float64(7), "is_pull_request": true, "comment": "/icaro review"}},
		{"gitlab", "gitlab_merge_request.json", "merge_request.opened", func(s string, b []byte) map[string]string {
			return map[string]string{"X-Gitlab-Token": s, "X-Gitlab-Event": "Merge Request Hook", "X-Gitlab-Event-UUID": "g1"}
		}, map[string]any{"mr_iid": float64(12), "project_id": float64(15), "sha": "da1560886d4f094c3e6c9ef40349f7d38b5d27d7"}},
		{"gitlab", "gitlab_note_mr.json", "note.merge_request", func(s string, b []byte) map[string]string {
			return map[string]string{"X-Gitlab-Token": s, "X-Gitlab-Event-UUID": "g2"}
		}, map[string]any{"mr_iid": float64(12), "note": "LGTM"}},
		{"jira", "jira_issue_created.json", "issue.created", hmacHeader("X-Hub-Signature", "sha256="),
			map[string]any{"issue_key": "ENG-12", "issue_type": "Bug", "project": "ENG"}},
		{"linear", "linear_issue_create.json", "issue.created", hmacHeader("Linear-Signature", ""),
			map[string]any{"identifier": "ENG-123", "team_id": "72b2a2dc-6f4f-4423-9d34-24b5bd10634a"}},
		{"sentry", "sentry_issue_created.json", "issue.created", func(s string, b []byte) map[string]string {
			return map[string]string{"Sentry-Hook-Resource": "issue", "Sentry-Hook-Signature": sign(s, string(b)), "Sentry-Hook-Timestamp": "1760954400", "Request-ID": "r1"}
		}, map[string]any{"issue_id": "1170820242", "project": "backend", "level": "error"}},
		{"datadog", "datadog_monitor_alert.json", "monitor.triggered", func(s string, b []byte) map[string]string {
			return map[string]string{"X-Icaro-Webhook-Secret": s}
		}, map[string]any{"monitor_id": "123456", "transition": "Triggered", "priority": "P2"}},
	}
	for _, c := range cases {
		t.Run(c.integration+"/"+c.event, func(t *testing.T) {
			spec := &workflow.Spec{Version: 1, Name: "w", Triggers: []workflow.TriggerSpec{{Integration: c.integration, Event: c.event}}}
			f := newHookFixture(t, fakeSpecs{spec}, config.WebhooksConfig{})
			f.secret(c.integration+".webhook_secret", "fixture-secret-123")
			body := fixture(t, c.fixture)
			code, out, raw := f.post("/hooks/"+c.integration, body, c.headers("fixture-secret-123", body))
			if code != http.StatusAccepted || f.engine.count() != 1 {
				t.Fatalf("%d %s", code, raw)
			}
			for _, e := range out["events"].([]any) {
				if msg, _ := e.(map[string]any)["error"].(string); msg != "" {
					t.Fatalf("event error: %s", msg)
				}
			}
			got := f.engine.last().Trigger
			for k, v := range c.check {
				if got[k] != v {
					t.Errorf("trigger.%s = %#v, want %#v (payload %v)", k, got[k], v, got)
				}
			}
			// Manual runs prompt for the same fields.
			ev, ok := f.hub.Registry().Snapshot().Event(c.integration, c.event)
			if !ok || len(ev.Fields) != len(got) {
				t.Fatalf("fields %v vs payload %v", ev.Fields, got)
			}
			if c.integration == "gitlab" {
				d, _ := f.hub.Delivery(context.Background(), out["delivery_id"].(string))
				if d.Headers["X-Gitlab-Token"][0] != "***" {
					t.Fatal("shared-secret header must be masked in the delivery record")
				}
			}
		})
	}
}

func TestGenericWebhookTokens(t *testing.T) {
	spec := &workflow.Spec{Version: 1, Name: "ingest", Triggers: []workflow.TriggerSpec{{Integration: "webhook", Event: "received", Filter: "trigger.body.kind == 'order'"}}}
	f := newHookFixture(t, fakeSpecs{spec}, config.WebhooksConfig{})
	ctx := context.Background()
	toks, err := f.hub.SyncWebhookTokens(ctx)
	if err != nil || len(toks) != 1 || toks[0].Workflow != "ingest" || len(toks[0].Token) < 20 {
		t.Fatalf("%+v %v", toks, err)
	}
	code, out, raw := f.post("/hooks/webhook/"+toks[0].Token+"?src=shop", []byte(`{"kind":"order","id":7}`), nil)
	if code != http.StatusAccepted || f.engine.count() != 1 {
		t.Fatalf("%d %s", code, raw)
	}
	r := f.engine.last()
	if r.Trigger["body"].(map[string]any)["id"] != float64(7) || r.Trigger["query"].(map[string]any)["src"] != "shop" {
		t.Fatalf("%v", r.Trigger)
	}
	// Identical unsigned requests are distinct deliveries.
	f.post("/hooks/webhook/"+toks[0].Token, []byte(`{"kind":"order","id":7}`), nil)
	if f.engine.count() != 2 {
		t.Fatal("unsigned requests without a delivery id must not dedupe")
	}
	_ = out
	if code, _, _ := f.post("/hooks/webhook/not-a-token", []byte(`{}`), nil); code != http.StatusNotFound {
		t.Fatal(code)
	}
	if code, _, _ := f.post("/hooks/webhook", []byte(`{}`), nil); code != http.StatusNotFound {
		t.Fatal(code)
	}
	rot, err := f.hub.RotateWebhookToken(ctx, "ingest")
	if err != nil || rot.Token == toks[0].Token {
		t.Fatal(err)
	}
	if code, _, _ := f.post("/hooks/webhook/"+toks[0].Token, []byte(`{}`), nil); code != http.StatusNotFound {
		t.Fatal("rotated token must stop working")
	}
}

func TestWebhookLimits(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{MaxBodyBytes: 64, RatePerMin: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.hub.limiter.run(ctx)
	for !f.hub.limiter.running.Load() {
		time.Sleep(time.Millisecond)
	}
	f.secret("github.webhook_secret", "s3cret-hook")
	big := bytes.Repeat([]byte("x"), 100)
	if code, _, _ := f.post("/hooks/github", big, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatal(code)
	}
	if code, _, _ := f.post("/hooks/github", []byte(`{}`), nil); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	if code, _, _ := f.post("/hooks/github", []byte(`{}`), nil); code != http.StatusTooManyRequests {
		t.Fatal("rate limit", code)
	}
	f.now = f.now.Add(time.Minute)
	if code, _, _ := f.post("/hooks/github", []byte(`{}`), nil); code == http.StatusTooManyRequests {
		t.Fatal("bucket must refill")
	}
}

// The delivery id is an unsigned header: a replay of a captured request
// with a fresh one must still be a duplicate, because the signed material
// is the same. Sentry's timestamp header is not signed either, so a
// refreshed timestamp does not make a replay new.
func TestWebhookReplayWithFreshUnsignedHeadersIsDuplicate(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	body := fixture(t, "github_pull_request_opened.json")
	_, out, raw := f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if out["status"] != store.DeliveryProcessed {
		t.Fatal(raw)
	}
	first := out["delivery_id"]
	code, out, _ := f.post("/hooks/github", body, ghHeaders("pull_request", "d-forged", "s3cret-hook", body))
	if code != http.StatusOK || out["status"] != store.DeliveryDuplicate || out["duplicate_of"] != first || f.engine.count() != 1 {
		t.Fatalf("replay with a fresh delivery id: %d %v (runs %d)", code, out, f.engine.count())
	}

	f.secret("sentry.webhook_secret", "sentry-secret")
	ev := fixture(t, "sentry_issue_created.json")
	sentryHdr := func(id string, at time.Time) map[string]string {
		return map[string]string{"Sentry-Hook-Resource": "issue", "Request-ID": id, "Sentry-Hook-Timestamp": strconv.FormatInt(at.Unix(), 10), "Sentry-Hook-Signature": sign("sentry-secret", string(ev))}
	}
	_, out, raw = f.post("/hooks/sentry", ev, sentryHdr("r-1", f.now))
	if out["status"] != store.DeliveryNoMatch {
		t.Fatal(raw)
	}
	f.now = f.now.Add(time.Hour) // past the skew of the original timestamp
	_, out, raw = f.post("/hooks/sentry", ev, sentryHdr("r-2", f.now))
	if out["status"] != store.DeliveryDuplicate {
		t.Fatalf("replay with a refreshed unsigned timestamp: %s", raw)
	}
}

// A rejected request is unauthenticated: only a prefix of its body is
// stored, with the full size and hash.
func TestRejectedDeliveryKeepsOnlyABodyPrefix(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	big := bytes.Repeat([]byte("A"), 10<<10)
	code, out, _ := f.post("/hooks/github", big, ghHeaders("pull_request", "d-x", "wrong", big))
	if code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	d, err := f.store.GetDelivery(context.Background(), out["delivery_id"].(string))
	sum := sha256.Sum256(big)
	if err != nil || len(d.Body) != rejectedBodyPrefix || d.BodySize != int64(len(big)) || d.BodySHA256 != hex.EncodeToString(sum[:]) || d.SignedSHA256 != "" {
		t.Fatalf("%d bytes stored, size %d, %v", len(d.Body), d.BodySize, err)
	}
	info, err := f.hub.Delivery(context.Background(), d.ID)
	if err != nil || !info.BodyTruncated || info.BodySize != int64(len(big)) {
		t.Fatalf("%+v %v", info, err)
	}
	// A verified delivery keeps its whole body.
	body := fixture(t, "github_pull_request_opened.json")
	_, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-y", "s3cret-hook", body))
	info, _ = f.hub.Delivery(context.Background(), out["delivery_id"].(string))
	if info.BodyTruncated || info.Body != string(body) {
		t.Fatalf("%+v", info)
	}
}

// webhooks.retention ages out delivery records and webhooks.max_rejected
// caps the rejected ones.
func TestHubPrunesDeliveries(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{reviewSpec}, config.WebhooksConfig{Retention: 24 * time.Hour, MaxRejected: 1})
	ctx := context.Background()
	f.secret("github.webhook_secret", "s3cret-hook")
	body := fixture(t, "github_pull_request_opened.json")
	for range 3 {
		f.post("/hooks/github", body, ghHeaders("pull_request", "d-z", "wrong", body))
	}
	if n, err := f.hub.PruneDeliveries(ctx); err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	f.now = time.Now().Add(48 * time.Hour) // store timestamps use the wall clock
	if n, err := f.hub.PruneDeliveries(ctx); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if def := NewHub(HubOptions{Store: f.store}); def.retention != 30*24*time.Hour || def.maxRejected != 1000 {
		t.Fatalf("defaults: %v %d", def.retention, def.maxRejected)
	}
}

