package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseT(t *testing.T, src string, opts Options) (*Workflow, Issues) {
	t.Helper()
	wf, issues, err := Parse([]byte(src), opts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return wf, issues
}

func mustHave(t *testing.T, issues Issues, path, msgPart, hintPart string) {
	t.Helper()
	for _, is := range issues {
		if is.Path == path && strings.Contains(is.Message, msgPart) && strings.Contains(is.Hint, hintPart) {
			return
		}
	}
	t.Fatalf("missing issue path=%q msg~%q hint~%q in:\n%s", path, msgPart, hintPart, issues.Error())
}

func TestParseValid(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wf, issues := parseT(t, string(src), Options{
		ConnectionExists: func(n string) bool { return n == "notify-api" },
		MaxTimeout:       time.Hour,
	})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %s", issues.Error())
	}
	if wf.Name != "notify-on-signup" || len(wf.Steps) != 2 || wf.Steps[1].Kind() != KindHTTP {
		t.Fatalf("decoded wrong: %+v", wf)
	}
	if wf.On.Webhook.Secret != "required" || wf.On.Schedule[0].Cron != "*/15 * * * *" {
		t.Fatalf("triggers wrong: %+v", wf.On)
	}
}

func TestUnknownFieldHint(t *testing.T) {
	_, issues := parseT(t, `
name: x
steps:
  - name: a
    run:
      imgae: alpine
      script: echo
`, Options{})
	mustHave(t, issues, "/steps/0/run/imgae", `unknown field "imgae"`, `"image"`)
}

func TestMissingRequired(t *testing.T) {
	_, issues := parseT(t, "steps: []\n", Options{})
	mustHave(t, issues, "/", "missing required field(s): name", "")
	mustHave(t, issues, "/steps", "", "")
}

func TestEnumHint(t *testing.T) {
	_, issues := parseT(t, `
name: x
steps:
  - name: a
    run: { image: alpine, script: echo }
    sandbox: stric
`, Options{})
	mustHave(t, issues, "/steps/0/sandbox", "not one of the allowed values", `"strict"`)
}

func TestSemanticRules(t *testing.T) {
	_, issues := parseT(t, `
name: x
on:
  schedule:
    - cron: "every day"
steps:
  - name: a
    run: { image: alpine, script: echo, command: [ls] }
    timeout: 2h
  - name: a
    http: { url: "https://x", auth: bearer, expect_status: [999] }
  - name: both
    run: { image: alpine, script: echo }
    http: { url: "https://x" }
  - name: none
    retry: { attempts: 2, backoff: soon }
  - name: ref
    run:
      image: alpine
      script: "{{ steps.aa.outputs.v }} {{ steps.later.outputs.v }}"
    env:
      BAD: "{{ inputs.x "
    connection: missing
  - name: later
    run: { image: alpine, command: ["true"] }
`, Options{ConnectionExists: func(string) bool { return false }, MaxTimeout: time.Hour})

	mustHave(t, issues, "/on/schedule/0/cron", "invalid cron", "")
	mustHave(t, issues, "/steps/0/run", "exactly one of: script, command", "")
	mustHave(t, issues, "/steps/0/timeout", "exceeds the server maximum", "")
	mustHave(t, issues, "/steps/1/name", "duplicate step name", "")
	mustHave(t, issues, "/steps/1/http/auth", "requires a step connection", "")
	mustHave(t, issues, "/steps/1/http/expect_status/0", "between 100 and 599", "")
	mustHave(t, issues, "/steps/2", "exactly one of: run, http", "")
	mustHave(t, issues, "/steps/3", "exactly one of: run, http", "")
	mustHave(t, issues, "/steps/3/retry/backoff", "Go duration", "")
	mustHave(t, issues, "/steps/4/run/script", "steps.aa, which is not an earlier step", "steps.a?")
	mustHave(t, issues, "/steps/4/run/script", "steps.later, which is not an earlier step", "")
	mustHave(t, issues, "/steps/4/env/BAD", "template error", "")
	mustHave(t, issues, "/steps/4/connection", `unknown connection "missing"`, "")
}

func TestParseNotMapping(t *testing.T) {
	_, issues := parseT(t, "- a\n", Options{})
	mustHave(t, issues, "/", "must be a mapping", "")
	_, issues = parseT(t, "", Options{})
	mustHave(t, issues, "/", "empty", "")
}

func TestParseInvalidYAML(t *testing.T) {
	if _, _, err := Parse([]byte("name: [\n"), Options{}); err == nil {
		t.Fatal("expected yaml error")
	}
}
