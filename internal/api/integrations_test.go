package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/apiclient"
	"icaro/internal/crypto"
	"icaro/internal/integration"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

type recEngine struct{ reqs []workflow.EnqueueRequest }

func (r *recEngine) EnqueueRun(_ context.Context, req workflow.EnqueueRequest) (workflow.EnqueueResult, error) {
	r.reqs = append(r.reqs, req)
	return workflow.EnqueueResult{RunID: fmt.Sprintf("r%d", len(r.reqs))}, nil
}

type oneSpec struct{}

func (oneSpec) ActiveSpecs(context.Context) ([]*workflow.Spec, error) {
	return []*workflow.Spec{{Version: 1, Name: "triage", Triggers: []workflow.TriggerSpec{{Integration: "github", Event: "issue_comment.created"}}}}, nil
}

func TestIntegrationRoutes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	sec := secrets.New(st, key)
	hub := integration.NewHub(integration.HubOptions{Dir: t.TempDir(), Store: st, Secrets: sec, Log: quiet})
	eng := &recEngine{}
	hub.SetEnqueuer(eng)
	hub.SetSpecSource(oneSpec{})
	srv := New(service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet}), quiet)
	srv.IntegrationRoutes(hub, sec, nil, "https://icaro.example.com")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	plain, hash, _ := crypto.NewToken()
	if _, err := st.CreateToken(ctx, "admin", hash, store.ScopeAdmin); err != nil {
		t.Fatal(err)
	}
	c := apiclient.New(hs.URL, plain)
	anon := apiclient.New(hs.URL, "")

	if _, err := anon.Integrations(ctx); err == nil {
		t.Fatal("API requires a token over TCP")
	}
	ins, err := c.Integrations(ctx)
	if err != nil || len(ins) != 8 {
		t.Fatalf("%d %v", len(ins), err)
	}
	gh, err := c.Integration(ctx, "github")
	if err != nil || !strings.Contains(gh.Manifest, "X-Hub-Signature-256") || len(gh.EventsFull["pull_request.opened"].Fields) == 0 {
		t.Fatalf("%v", err)
	}
	st1, _ := c.IntegrationStatus(ctx)
	if len(st1.MissingSecrets) != 1 || st1.MissingSecrets[0] != "github.webhook_secret" {
		t.Fatalf("missing secrets: %+v", st1)
	}

	plan, err := c.PlanCredential(ctx, "github", "token")
	if err != nil || plan.Hosts[0] != "api.github.com" || plan.Webhook {
		t.Fatalf("%+v %v", plan, err)
	}
	if _, err := c.PlanCredential(ctx, "github", "tokn"); err == nil || !strings.Contains(err.Error(), "uses no credential") {
		t.Fatal(err)
	}
	if _, err := c.AddCredential(ctx, "github", "token", "ghp_abcdefghijklmnopqrstuvwxyz012345", nil); err != nil {
		t.Fatal(err)
	}
	wh, err := c.AddCredential(ctx, "github", "webhook_secret", "hook-secret-xyz", []string{"ignored.example"})
	if err != nil || !wh.Webhook || len(wh.Hosts) != 0 {
		t.Fatalf("%+v %v", wh, err)
	}
	var raw []byte
	if err := c.Do(ctx, http.MethodGet, "/api/v1/credentials", nil, &raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("ghp_")) || bytes.Contains(raw, []byte("hook-secret")) || !bytes.Contains(raw, []byte("github.token")) {
		t.Fatalf("credential list must not include values: %s", raw)
	}

	// An invalid manifest edit is rejected with structured issues.
	_, err = c.EditManifest(ctx, "acme", "version: 1\nname: acme\nbogus: 1\n")
	var ae *apiclient.Error
	if !asAPIError(err, &ae) || ae.Status != http.StatusUnprocessableEntity || len(ae.Issues) == 0 || ae.Issues[0].Path != "/bogus" {
		t.Fatalf("%v", err)
	}
	res, err := c.EditManifest(ctx, "acme", "version: 1\nname: acme\ndescription: x\n")
	if err != nil || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}

	// Webhook in, delivery recorded, redelivered.
	body := []byte(`{"action":"created","issue":{"number":3},"comment":{"body":"hi","html_url":"u","user":{"login":"a"}},"repository":{"full_name":"o/r"}}`)
	m := hmac.New(sha256.New, []byte("hook-secret-xyz"))
	m.Write(body)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/hooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "dlv-1")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("hook: %v %v", resp.Status, err)
	}
	_ = resp.Body.Close()
	if len(eng.reqs) != 1 || eng.reqs[0].Trigger["is_pull_request"] != false {
		t.Fatalf("%+v", eng.reqs)
	}
	ds, err := c.Deliveries(ctx, "github", "", 10)
	if err != nil || len(ds) != 1 || ds[0].Status != store.DeliveryProcessed {
		t.Fatalf("%+v %v", ds, err)
	}
	d, err := c.Delivery(ctx, ds[0].ID)
	if err != nil || !strings.Contains(d.Body, `"number":3`) {
		t.Fatal(err)
	}
	out, err := c.Redeliver(ctx, ds[0].ID)
	if err != nil || out.Status != store.DeliveryProcessed || len(eng.reqs) != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := c.Delivery(ctx, "nope"); !apiclient.IsNotFound(err) {
		t.Fatal(err)
	}
	var schema []byte
	if err := c.Do(ctx, http.MethodGet, "/api/v1/actions/github.create-issue/schema", nil, &schema); err != nil || !bytes.Contains(schema, []byte(`"required"`)) {
		t.Fatalf("%s %v", schema, err)
	}
	toks, err := c.WebhookTokens(ctx)
	if err != nil || len(toks) != 0 {
		t.Fatal(toks, err)
	}
	if err := c.DeleteCredential(ctx, "github.token"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCredential(ctx, "github.token"); !apiclient.IsNotFound(err) {
		t.Fatal(err)
	}
}

func asAPIError(err error, target **apiclient.Error) bool {
	e, ok := err.(*apiclient.Error)
	if ok {
		*target = e
	}
	return ok
}
