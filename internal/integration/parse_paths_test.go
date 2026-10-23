package integration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"icaro/internal/doctor"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

func TestParseSemanticBranches(t *testing.T) {
	src := `version: 1
name: sem
hosts: [api.sem.dev]
auth:
  open: { kind: none }
  bare: { kind: bearer }
  basic: { kind: basic, credential: pass, username: "${{ body.user }}" }
triggers:
  verify: { kind: url-token, header: X-Nope }
  events:
    e:
      match: "body.x ==="
      payload: { x: "${{ input.x }}" }
actions:
  form:
    request:
      method: POST
      url: "https://api.sem.dev/f"
      body: { form: { a: "${{ input.missing }}" } }
  unclosed:
    request: { method: GET, url: "${{ config.base_url" }
  dynhost:
    inputs: { h: { type: string } }
    request: { method: GET, url: "https://${{ input.h }}.sem.dev/" }
  typed:
    inputs:
      o: { type: object, default: { a: 1 } }
      n: { type: number, default: 2 }
    request: { method: GET, url: "https://api.sem.dev/t" }
`
	_, issues, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	text := issues.Error()
	for _, want := range []string{
		`/auth/bare/credential: auth kind "bearer" needs a credential`,
		"/auth/basic/username",
		"/triggers/verify: url-token verification takes no header or message",
		"/triggers/events/e/match: expression error",
		"/triggers/events/e/payload/x",
		"/actions/form/request/body/form/a: references input.missing",
		"/actions/unclosed/request/url: expression error",
		"/actions/dynhost/request/url: the URL must start with a literal scheme and host",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, strings.ReplaceAll(text, "; ", "\n"))
		}
	}
	if strings.Contains(text, "/auth/open") || strings.Contains(text, "/actions/typed") {
		t.Fatalf("valid entries must not be reported:\n%s", text)
	}
}

func TestParseDecodeAndTypes(t *testing.T) {
	// Schema-invalid and undecodable: the schema issue is what is reported.
	// Issues come in no fixed order.
	_, issues, err := Parse([]byte("version: one\nname: x\n"))
	if err != nil || !slices.ContainsFunc(issues, func(i Issue) bool { return i.Path == "/version" }) {
		t.Fatalf("%v %v", issues, err)
	}
	// Non-string mapping keys are normalized to strings.
	if v := normalizeYAML(map[any]any{1: map[any]any{true: 2}}); v.(map[string]any)["1"].(map[string]any)["true"] != float64(2) {
		t.Fatalf("%#v", v)
	}
	for _, c := range []struct {
		typ string
		v   any
		ok  bool
	}{
		{"object", map[string]any{}, true},
		{"object", "x", false},
		{"array", []any{}, true},
		{"number", int64(3), true},
		{"date", "2026-01-01", false},
	} {
		if typeMatches(c.typ, c.v) != c.ok {
			t.Errorf("typeMatches(%s, %#v) != %v", c.typ, c.v, c.ok)
		}
	}
	if errText(os.ErrNotExist, nil) != os.ErrNotExist.Error() {
		t.Fatal("errText prefers the error")
	}
}

func TestDoctorChecksEdgeCases(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// A user pack shadowing a builtin is a warning in the detail.
	_ = os.MkdirAll(filepath.Join(dir, "mine"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "mine", "slack.yaml"), []byte("version: 1\nname: slack\nhosts: [slack.com]\n"), 0o600)
	reps := doctor.Run(ctx, DoctorChecks(dir, db), false)
	if reps[0].Severity != doctor.OK || !strings.Contains(reps[0].Detail, "shadows the builtin slack") {
		t.Fatalf("%+v", reps[0])
	}

	// Manual triggers, unknown integrations, url-token integrations and
	// integrations without triggers need no webhook secret; a custom
	// verification credential name is honored.
	probeDir := filepath.Join(dir, "probe")
	_ = os.MkdirAll(probeDir, 0o700)
	_ = os.WriteFile(filepath.Join(probeDir, "probe.yaml"), []byte(probeManifest), 0o600)
	snap := NewRegistry(dir, nil).Snapshot()
	specs := fakeSpecs{{Version: 1, Name: "w", Triggers: []workflow.TriggerSpec{
		{Type: "manual"},
		{Integration: "nope", Event: "x"},
		{Integration: "webhook", Event: "received"},
		{Integration: "slack", Event: "x"},
		{Integration: "probe", Event: "thing"},
	}}}
	missing, err := MissingWebhookSecrets(ctx, snap, specs, db)
	if err != nil || strings.Join(missing, ",") != "probe.signing" {
		t.Fatalf("%v %v", missing, err)
	}
	if _, err := MissingWebhookSecrets(ctx, snap, failingSpecSource{}, db); err == nil {
		t.Fatal("spec source errors are returned")
	}
	_ = db.Close()
	if _, err := MissingWebhookSecrets(ctx, snap, specs, db); err == nil {
		t.Fatal("store errors are returned")
	}
	reps = doctor.Run(ctx, DoctorChecks(dir, db), false)
	if reps[1].Severity != doctor.Error {
		t.Fatalf("an unreadable store is an error: %+v", reps[1])
	}
}
