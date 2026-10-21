package integration

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/secrets"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

func TestHubAccessorsAndCatalog(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	if f.hub.Name() != "integrations" || f.hub.Packs() == nil || f.hub.Executor() == nil {
		t.Fatal("accessors")
	}
	cat := f.hub.Catalog()
	if a, ok := cat.Action("github.create-issue-comment"); !ok || a.Ref != "github.create-issue-comment" {
		t.Fatalf("catalog action: %+v %v (refs %v)", a, ok, cat.ActionRefs())
	}
	if e, ok := cat.Event("github", "pull_request.opened"); !ok || len(e.Fields) == 0 {
		t.Fatalf("catalog event: %+v", e)
	}
	if len(cat.EventNames("github")) == 0 || len(cat.Integrations()) < 8 {
		t.Fatal("catalog lists")
	}
	snap := f.hub.Registry().Snapshot()
	for _, ref := range []string{"nodot", "nope.x", "github.nope"} {
		if _, ok := snap.Action(ref); ok {
			t.Errorf("%s must not resolve", ref)
		}
	}
	if _, ok := snap.Event("nope", "x"); ok {
		t.Error("unknown integration")
	}
	if _, ok := snap.Event("github", "nope"); ok {
		t.Error("unknown event")
	}
	if snap.EventNames("nope") != nil {
		t.Error("unknown integration has no events")
	}
	if _, ok := snap.ActionInputSchema("nope.x"); ok {
		t.Error("unknown action has no schema")
	}

	schemas := f.hub.ActionInputSchemas()
	s, ok := schemas["github.create-issue-comment"]
	if !ok || s["title"] != "github.create-issue-comment input" || len(s["required"].([]string)) == 0 {
		t.Fatalf("schemas: %v", s)
	}

	all := f.hub.Integrations()
	if len(all) < 8 || all[0].Manifest != "" {
		t.Fatal("the list carries no manifest source")
	}
	gh, err := f.hub.Integration("github")
	if err != nil || gh.Manifest == "" || len(gh.EventsFull) == 0 || len(gh.ActionsFull) == 0 || gh.Pack != PackBuiltin {
		t.Fatalf("%+v %v", gh, err)
	}
	if _, err := f.hub.Integration("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestActionInputSchemaDescribesInputs(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "p"), 0o700)
	src := `version: 1
name: sch
hosts: [api.sch.dev]
actions:
  go:
    description: Go somewhere.
    inputs:
      where: { type: string, required: true, description: Destination. }
      speed: { type: number, default: 3 }
    request: { method: POST, url: "https://api.sch.dev/go" }
`
	_ = os.WriteFile(filepath.Join(dir, "p", "sch.yaml"), []byte(src), 0o600)
	snap := NewRegistry(dir, nil).Snapshot()
	s, ok := snap.ActionInputSchema("sch.go")
	if !ok {
		t.Fatal(snap.Errors())
	}
	props := s["properties"].(map[string]any)
	where := props["where"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	speed := props["speed"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	if where["description"] != "Destination." || speed["default"] != float64(3) || s["description"] != "Go somewhere." {
		t.Fatalf("%v", s)
	}
}

func TestStoreSpecsSkipsInactiveAndInvalid(t *testing.T) {
	f := newHookFixture(t, nil, config.WebhooksConfig{})
	ctx := context.Background()
	apply := func(name, yaml string) *store.Workflow {
		w, err := f.store.ApplyWorkflow(ctx, name, yaml, "{}", "t")
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	apply("zeta", "version: 1\nname: zeta\nsteps: []\n")
	apply("alpha", "version: 1\nsteps: []\n") // name comes from the row
	apply("broken", "version: [\n")
	apply("future", "version: 2\nname: future\n")
	off := apply("off", "version: 1\nname: off\n")
	if err := f.store.SetWorkflowStatus(ctx, off.ID, store.WorkflowDisabled); err != nil {
		t.Fatal(err)
	}
	specs, err := StoreSpecs{Store: f.store}.ActiveSpecs(ctx)
	if err != nil || len(specs) != 2 || specs[0].Name != "alpha" || specs[1].Name != "zeta" {
		t.Fatalf("%+v %v", specs, err)
	}
	_ = f.store.Close()
	if _, err := (StoreSpecs{Store: f.store}).ActiveSpecs(ctx); err == nil {
		t.Fatal("store errors are returned")
	}
}

func TestWebhookTokenErrors(t *testing.T) {
	ingest := &workflow.Spec{Version: 1, Name: "ingest", Triggers: []workflow.TriggerSpec{{Integration: "webhook", Event: "received"}}}
	f := newHookFixture(t, fakeSpecs{ingest}, config.WebhooksConfig{})
	ctx := context.Background()
	if _, err := f.hub.RotateWebhookToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotating an unknown workflow's token: %v", err)
	}
	f.hub.SetSpecSource(failingSpecSource{})
	if _, err := f.hub.SyncWebhookTokens(ctx); err == nil {
		t.Fatal("spec source errors are returned")
	}
	f.hub.SetSpecSource(fakeSpecs{ingest})
	_ = f.store.Close()
	if _, err := f.hub.SyncWebhookTokens(ctx); err == nil {
		t.Fatal("store errors are returned")
	}
	if _, err := f.hub.RotateWebhookToken(ctx, "ingest"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a store error is not a not-found: %v", err)
	}
}

func TestServeHookTokenWithoutGenericWebhook(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	// A user pack replaces the builtin webhook integration with one that
	// has no triggers: tokenized URLs then have nothing to serve.
	f.install(map[string]string{"webhook": "version: 1\nname: webhook\nhosts: [x.example]\n"})
	if w := f.hub.Registry().Snapshot().Warnings; len(w) != 1 || !strings.Contains(w[0], "shadows the builtin webhook") {
		t.Fatal(w)
	}
	code, out, _ := f.post("/hooks/webhook/tok", []byte(`{}`), nil)
	if code != http.StatusNotFound || !strings.Contains(out["error"].(string), "not loaded") {
		t.Fatal(code, out)
	}
}

func TestHubRunWatchesPacks(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	f.hub.SetSpecSource(failingSpecSource{}) // token sync fails and is logged
	f.hub.poll = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.hub.Run(ctx) }()
	reg := f.hub.Registry()
	deadline := time.Now().Add(3 * time.Second)
	for !reg.running.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// A new manifest is picked up by the poller without an explicit reload.
	dir := filepath.Join(reg.Dir(), "watched")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte(acmeManifest), 0o600)
	for time.Now().Before(deadline) {
		if _, ok := reg.Snapshot().Get("acme"); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := reg.Snapshot().Get("acme"); !ok {
		t.Fatal("the pack watcher must load new manifests")
	}
	// Reload while running goes through the owner goroutine.
	_ = os.Remove(filepath.Join(dir, "acme.yaml"))
	if err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Snapshot().Get("acme"); ok {
		t.Fatal("reload must drop the removed manifest")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A cancelled context ends a pending reload.
	reg.running.Store(true)
	defer reg.running.Store(false)
	if err := reg.Reload(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("reload with a cancelled context: %v", err)
	}
}

func TestRegistryRunDefaultInterval(t *testing.T) {
	reg := NewRegistry(t.TempDir(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- reg.Run(ctx, 0) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if NewRegistry(filepath.Join(t.TempDir(), "missing"), nil).Snapshot().Names() == nil {
		t.Fatal("a missing directory still serves the builtins")
	}
}

func TestReloadWaitsForTheOwner(t *testing.T) {
	reg := NewRegistry(t.TempDir(), nil)
	// The owner goroutine takes the request but has not answered yet.
	reg.running.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error)
	go func() { errc <- reg.Reload(ctx) }()
	<-reg.reload // accepted, never answered
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCredentialAdmin(t *testing.T) {
	ingest := &workflow.Spec{Version: 1, Name: "review", Triggers: []workflow.TriggerSpec{{Integration: "github", Event: "pull_request.opened"}}}
	f := newHookFixture(t, fakeSpecs{ingest}, config.WebhooksConfig{})
	ctx := context.Background()

	if _, err := f.hub.PlanCredential("nope", "token"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	plan, err := f.hub.AddCredential(ctx, f.secrets, "github", "token", "ghp-value-123456", nil)
	if err != nil || plan.Secret != "github.token" || len(plan.Hosts) == 0 {
		t.Fatalf("%+v %v", plan, err)
	}
	plan, err = f.hub.AddCredential(ctx, f.secrets, "github", "token", "ghp-value-123456", []string{"ghe.corp.example"})
	if err != nil || strings.Join(plan.Hosts, ",") != "ghe.corp.example" {
		t.Fatalf("explicit hosts replace the default binding: %+v %v", plan, err)
	}
	if _, err := f.hub.AddCredential(ctx, f.secrets, "github", "token", "v", []string{}); err == nil {
		t.Fatal("an action credential needs a host")
	}
	// A webhook secret is never sent anywhere: explicit hosts are ignored.
	plan, err = f.hub.AddCredential(ctx, f.secrets, "github", "webhook_secret", "hook-123456", []string{"x.example"})
	if err != nil || !plan.Webhook || len(plan.Hosts) != 0 {
		t.Fatalf("%+v %v", plan, err)
	}
	if _, err := f.hub.AddCredential(ctx, f.secrets, "github", "nope", "v", nil); err == nil {
		t.Fatal("unknown credential")
	}
	if _, err := f.hub.AddCredential(ctx, failingSetter{}, "github", "token", "v", nil); err == nil {
		t.Fatal("set errors are returned")
	}

	_ = f.secrets.Set(ctx, "plain", "not-an-integration", nil)
	_ = f.secrets.Set(ctx, "gone.token", "unloaded-integration", nil)
	creds, err := f.hub.Credentials(ctx, f.secrets)
	if err != nil || len(creds) != 2 || creds[0].Name != "github.token" || creds[1].Name != "github.webhook_secret" {
		t.Fatalf("only loaded integrations' credentials are listed: %+v %v", creds, err)
	}
	if _, err := f.hub.Credentials(ctx, failingSetter{}); err == nil {
		t.Fatal("list errors are returned")
	}

	st := f.hub.Status(ctx)
	if len(st.MissingSecrets) != 0 || len(st.Packs) == 0 || st.LoadedAt == "" {
		t.Fatalf("%+v", st)
	}
	_ = f.secrets.Delete(ctx, "github.webhook_secret")
	if st := f.hub.Status(ctx); strings.Join(st.MissingSecrets, ",") != "github.webhook_secret" {
		t.Fatalf("%+v", st)
	}
}

type failingSetter struct{}

func (failingSetter) Set(context.Context, string, string, []string) error {
	return errors.New("locked")
}
func (failingSetter) List(context.Context) ([]secrets.Info, error) { return nil, errors.New("locked") }
func (failingSetter) Delete(context.Context, string) error         { return errors.New("locked") }

func TestHubEditManifest(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	ctx := context.Background()
	commit, err := f.hub.EditManifest(ctx, "acme", []byte(acmeManifest), "")
	if err != nil || commit == "" {
		t.Fatal(commit, err)
	}
	if _, ok := f.hub.Registry().Snapshot().Get("acme"); !ok {
		t.Fatal("an edit serves immediately")
	}
	if _, err := f.hub.EditManifest(ctx, "acme", []byte("version: 2\n"), ""); err == nil {
		t.Fatal("invalid edits are rejected")
	}
}
