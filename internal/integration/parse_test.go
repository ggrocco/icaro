package integration

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestBuiltinManifestsAreValid(t *testing.T) {
	files := builtinFiles()
	if len(files) != 8 {
		t.Fatalf("want 8 builtin manifests, got %d", len(files))
	}
	for name, src := range files {
		m, issues, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(issues) > 0 {
			t.Fatalf("%s: %v", name, issues)
		}
		if m.Name+".yaml" != name {
			t.Fatalf("%s declares name %q", name, m.Name)
		}
	}
	want := []string{"datadog", "github", "gitlab", "jira", "linear", "sentry", "slack", "webhook"}
	if got := strings.Join(BuiltinNames(), ","); got != strings.Join(want, ",") {
		t.Fatal(got)
	}
}

func TestPublishedSchemaMatchesEmbedded(t *testing.T) {
	pub, err := os.ReadFile("../../blueprint/docs/schemas/integration-manifest.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub, SchemaJSON()) {
		t.Fatal("blueprint/docs/schemas/integration-manifest.schema.json drifted from internal/integration/manifest.schema.json")
	}
}

func TestParseIssues(t *testing.T) {
	src := `
version: 1
name: demo
hosts: [api.demo.dev]
auth:
  tok: { kind: header, credential: token }
  q: { kind: query, credential: key }
triggers:
  verify: { kind: hmac-sha256 }
  timestamp: { header: X-Ts, max_skew: 0s }
  events:
    thing:
      match: "bodyy.x == 1"
      payload:
        id: "${{ body.id "
actions:
  do:
    inputs:
      name: { type: string, default: 3 }
    request:
      method: POST
      url: "https://evil.example.com/${{ input.nmae }}"
      headers: { Authorization: "x" }
      auth: tk
      body: { json: { a: "${{ config.missing }}" }, raw: "x" }
    outputs:
      id: "${{ response.body.id }}"
    succes: "x"
  plain:
    request: { method: GET, url: "http://api.demo.dev/x" }
  dyn:
    request: { method: GET, url: "${{ input.u }}" }
`
	_, issues, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// Schema issues come first; fix the unknown field and re-check semantics.
	if len(issues) != 1 || issues[0].Path != "/actions/do/succes" {
		t.Fatalf("schema pass: %v", issues)
	}
	_, issues, _ = Parse([]byte(strings.Replace(strings.Replace(src, `    succes: "x"`+"\n", "", 1), "hosts: [api.demo.dev]", `hosts: [api.demo.dev, "*.dev"]`, 1)))
	text := issues.Error()
	for _, want := range []string{
		"/auth/tok/header: auth kind header needs a header name",
		"/auth/q/param",
		"/triggers/verify/header",
		"/triggers/timestamp/max_skew",
		`unknown name "bodyy"`,
		"/triggers/events/thing/payload/id: expression error",
		"/actions/do/inputs/name/default",
		"references input.nmae, which is not a declared input (hint: did you mean input.name?)",
		`host "evil.example.com" is not declared in hosts`,
		"/actions/do/request/headers/Authorization",
		`unknown auth "tk" (hint: did you mean "tok"?)`,
		"config.missing",
		"body needs exactly one of json, form, raw",
		"/actions/plain/request/url: action URLs must use https",
		"/actions/dyn/request/url: the URL must start with https://<host> or ${{ config.<key> }}",
		`/hosts/1: wildcard host "*.dev" covers a whole top-level domain`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, strings.ReplaceAll(text, "; ", "\n"))
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, _, err := Parse([]byte("a: [")); err == nil {
		t.Fatal("bad yaml")
	}
	_, issues, _ := Parse([]byte(""))
	if len(issues) != 1 {
		t.Fatal("empty")
	}
	_, issues, _ = Parse([]byte("- 1"))
	if len(issues) != 1 {
		t.Fatal("list")
	}
	_, issues, _ = Parse([]byte("version: 1\nname: x1\ntriggers: {verify: {kind: hmac-sha512}, events: {a: {match: 'true', payload: {}}}}"))
	if len(issues) == 0 || !strings.Contains(issues.Error(), "allowed") {
		t.Fatal(issues)
	}
}
