package workflow

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeCatalog struct{}

func (fakeCatalog) Action(ref string) (ActionInfo, bool) {
	if ref == "slack.post-message" {
		return ActionInfo{Ref: ref, Inputs: map[string]InputInfo{
			"channel": {Type: "string", Required: true},
			"text":    {Type: "string", Required: true},
			"blocks":  {Type: "array"},
		}, Outputs: []string{"ts", "channel"}}, true
	}
	return ActionInfo{}, false
}

func (fakeCatalog) Event(integration, event string) (EventInfo, bool) {
	if integration == "github" && event == "pull_request.opened" {
		return EventInfo{Integration: integration, Event: event, Fields: []string{"pr_number", "repo", "branch", "title"}}, true
	}
	return EventInfo{}, false
}
func (fakeCatalog) ActionRefs() []string { return []string{"slack.post-message"} }
func (fakeCatalog) EventNames(string) []string {
	return []string{"pull_request.opened", "push"}
}
func (fakeCatalog) Integrations() []string { return []string{"github", "slack"} }

func parseT(t *testing.T, src string, opts Options) (*Spec, Issues) {
	t.Helper()
	s, issues, err := Parse([]byte(src), opts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s, issues
}

func mustHave(t *testing.T, issues Issues, path, msgPart, hintPart string) {
	t.Helper()
	for _, is := range issues {
		if is.Path == path && strings.Contains(is.Message, msgPart) && strings.Contains(is.Hint, hintPart) {
			return
		}
	}
	t.Fatalf("missing issue path=%q msg~%q hint~%q in:\n%s", path, msgPart, hintPart, strings.ReplaceAll(issues.Error(), "; ", "\n"))
}

const validYAML = `
version: 1
name: review-pr
description: Review a PR and report to Slack
concurrency: queue
env:
  STAGE: prod
  TOKEN: ${{ secrets.gh_token }}
triggers:
  - type: manual
  - type: cron
    cron: "0 9 * * 1-5"
  - integration: github
    event: pull_request.opened
    filter: trigger.branch != 'main'
steps:
  - id: checkout
    image: mirror.gcr.io/library/alpine:3.20
    repository: { ref: github.com/acme/app, revision: main }
    command: echo ${{ trigger.repo }}
    outputs:
      sha: json:git.sha
  - id: lint
    image: mirror.gcr.io/library/alpine:3.20
    needs: [checkout]
    command: echo lint ${{ steps.checkout.outputs.sha }}
    timeout: 10m
    retry: 2
    outputs:
      report: stdout
  - id: test
    image: mirror.gcr.io/library/alpine:3.20
    needs: [checkout]
    env: { MODE: "${{ env.STAGE }}" }
    command: echo test
    outputs:
      summary: stdout:last-line
      junit: file:reports/junit.xml
  - id: notify
    uses: slack.post-message
    needs: [lint, test]
    when: ${{ steps.lint.status == 'succeeded' && trigger.pr_number > 0 }}
    input:
      channel: "#ci"
      text: "PR ${{ trigger.pr_number }}: ${{ steps.test.outputs.summary }}"
  - id: after
    image: mirror.gcr.io/library/alpine:3.20
    needs: [notify]
    command: echo ${{ steps.notify.outputs.ts }} ${{ steps.checkout.outputs.sha }}
`

func TestParseValid(t *testing.T) {
	s, issues := parseT(t, validYAML, Options{
		MaxTimeout:   time.Hour,
		Catalog:      fakeCatalog{},
		SecretExists: func(n string) bool { return n == "gh_token" },
	})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues:\n%s", strings.ReplaceAll(issues.Error(), "; ", "\n"))
	}
	if s.Name != "review-pr" || len(s.Steps) != 5 || s.Steps[3].Kind() != KindUses || s.ConcurrencyPolicy() != ConcurrencyQueue {
		t.Fatalf("decoded: %+v", s)
	}
	if got := s.CronTriggers(); len(got) != 1 || len(s.IntegrationTriggers()) != 1 {
		t.Fatalf("triggers %v", got)
	}
	req, opt := TriggerFields(s)
	if !reflect.DeepEqual(req, []string{"pr_number", "repo"}) || len(opt) != 0 {
		t.Fatalf("trigger fields %v %v", req, opt)
	}
	if got := SecretRefs(s); !reflect.DeepEqual(got, []string{"gh_token"}) {
		t.Fatalf("secret refs %v", got)
	}
	p, err := NewPlan(s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Order, []string{"checkout", "lint", "test", "notify", "after"}) || !p.Ancestors["after"]["checkout"] {
		t.Fatalf("plan %+v", p)
	}
	if !reflect.DeepEqual(p.Roots(), []string{"checkout"}) || p.Index()["notify"] != 3 {
		t.Fatal("roots/index")
	}
}

func TestSchemaIssues(t *testing.T) {
	src := `
version: 2
name: Bad_Name
concurency: queue
triggers:
  - type: crn
  - integration: github
    evnt: push
  - {}
steps:
  - id: a
    image: x
    uses: slack.post-message
  - id: b
  - id: c
    image: x
    volumes: ["/var/run/docker.sock:/var/run/docker.sock"]
    retry: 9
    timeout: 10 minutes
`
	_, issues := parseT(t, src, Options{})
	mustHave(t, issues, "/version", "must be 1", "")
	mustHave(t, issues, "/name", "does not match", "")
	mustHave(t, issues, "/concurency", `unknown field "concurency"`, `did you mean "concurrency"?`)
	mustHave(t, issues, "/triggers/0/type", "not one of the allowed values", `did you mean "cron"?`)
	mustHave(t, issues, "/triggers/1/evnt", `unknown field "evnt"`, `did you mean "event"?`)
	mustHave(t, issues, "/triggers/2", "a trigger needs type", "")
	mustHave(t, issues, "/steps/0", "not both", "")
	mustHave(t, issues, "/steps/1", "exactly one of: image, uses", "")
	// Dangerous mounts are not expressible: unknown fields are rejected.
	mustHave(t, issues, "/steps/2/volumes", `unknown field "volumes"`, "")
	mustHave(t, issues, "/steps/2/retry", "", "")
	mustHave(t, issues, "/steps/2/timeout", "does not match", "")
}

func TestSemanticIssues(t *testing.T) {
	src := `
version: 1
name: sem-test
env:
  GOOD: x
triggers:
  - type: cron
    cron: "61 * * * *"
  - type: manual
    cron: "* * * * *"
  - integration: githb
    event: push
  - integration: github
    event: pull_request.opend
  - integration: slack
    event: push
    filter: trigger.nope == 1 || tigger.x
steps:
  - id: aa
    image: x
    outputs:
      "bad name": stdout
      v: jsn:x
      ff: file:../etc/passwd
  - id: aa
    image: x
  - id: bb
    image: x
    needs: [aa, zz, bb]
  - id: cc
    image: x
    needs: [dd]
    timeout: 2h
  - id: dd
    image: x
    needs: [cc]
  - id: ee
    uses: slack.post-mesage
  - id: ff
    uses: slack.post-message
    command: x
    env: { A: bb }
    outputs: { o: stdout }
    repository: { ref: r }
    input:
      chanel: "#x"
      blocks: "not an array"
`
	_, issues := parseT(t, src, Options{MaxTimeout: time.Hour, Catalog: fakeCatalog{}})
	mustHave(t, issues, "/triggers/0/cron", "invalid cron expression", "5 fields")
	mustHave(t, issues, "/triggers/1/cron", "only valid on cron triggers", "")
	mustHave(t, issues, "/triggers/2/integration", `unknown integration "githb"`, `did you mean "github"?`)
	mustHave(t, issues, "/triggers/3/event", `no event "pull_request.opend"`, `did you mean "pull_request.opened"?`)
	mustHave(t, issues, "/triggers/4/filter", `unknown name "tigger"`, `did you mean "trigger"`)
	mustHave(t, issues, "/steps/0/outputs/bad name", "invalid output name", "")
	mustHave(t, issues, "/steps/0/outputs/v", "unknown output source", "json:<path>")
	mustHave(t, issues, "/steps/0/outputs/ff", "inside /workspace", "")
	mustHave(t, issues, "/steps/1/id", "duplicate step id", "")
	mustHave(t, issues, "/steps/2/needs/1", `unknown step "zz"`, "")
	mustHave(t, issues, "/steps/2/needs/2", "cannot need itself", "")
	mustHave(t, issues, "/steps/3/timeout", "exceeds the configured maximum", "")
	mustHave(t, issues, "/steps/5/uses", `unknown action "slack.post-mesage"`, `did you mean "slack.post-message"?`)
	mustHave(t, issues, "/steps/6/command", "only valid on image steps", "")
	mustHave(t, issues, "/steps/6/env", "only valid on image steps", "")
	mustHave(t, issues, "/steps/6/outputs", "declared by the action's manifest", "")
	mustHave(t, issues, "/steps/6/repository", "only valid on image steps", "")
	mustHave(t, issues, "/steps/6/input/chanel", `unknown input "chanel"`, `did you mean "channel"?`)
	mustHave(t, issues, "/steps/6/input/blocks", "must be of type array", "")
	mustHave(t, issues, "/steps/6/input", `missing required input "channel"`, "")
}

func TestCycleReported(t *testing.T) {
	src := `
version: 1
name: cyc
steps:
  - { id: aa, image: x, needs: [cc] }
  - { id: bb, image: x, needs: [aa] }
  - { id: cc, image: x, needs: [bb] }
`
	_, issues := parseT(t, src, Options{})
	mustHave(t, issues, "/steps/0/needs", "dependency cycle: aa -> cc -> bb -> aa", "remove one")
}

func TestExpressionIssues(t *testing.T) {
	src := `
version: 1
name: exprs
env:
  E1: ${{ steps.aa.outputs.x }}
triggers:
  - integration: github
    event: pull_request.opened
steps:
  - id: aa
    image: x
    outputs: { result: json:result }
  - id: bb
    image: x
    command: echo ${{ steps.aa.outputs.result }}
  - id: cc
    image: x
    needs: [aa]
    command: echo ${{ steps.aa.outputs.reslt }} ${{ stpes.aa }} ${{ steps.zz.outputs.q }}
    env:
      X: ${{ env.E2 }}
      Y: ${{ secrets.missing }}
      Z: ${{ trigger.pr_numbr }}
      W: ${{ steps.aa.otputs.x }}
      V: ${{ lower( }}
      U: ${{ has(trigger.optional) }}
    when: steps.aa.outputs.result == 'ok' && contans('a', 'b')
  - id: dd
    uses: slack.post-message
    needs: [aa]
    input:
      channel: x
      text: ${{ steps.aa.outputs.nope }}
  - id: ee
    image: x
    needs: [dd]
    command: ${{ steps.dd.outputs.tss }}
`
	_, issues := parseT(t, src, Options{Catalog: fakeCatalog{}, SecretExists: func(string) bool { return false }})
	mustHave(t, issues, "/env/E1", `unknown name "steps"`, "")
	mustHave(t, issues, "/steps/1/command", "not an ancestor of step bb", `add "aa" to needs`)
	mustHave(t, issues, "/steps/2/command", `declares no output "reslt"`, "did you mean steps.aa.outputs.result?")
	mustHave(t, issues, "/steps/2/command", `unknown name "stpes"`, `did you mean "steps"`)
	mustHave(t, issues, "/steps/2/command", "steps.zz: no such step", "")
	mustHave(t, issues, "/steps/2/env/X", "env.E2 is not a workflow env variable", "did you mean env.E1?")
	mustHave(t, issues, "/steps/2/env/Y", `secret "missing" does not exist`, "icaro secret set missing")
	mustHave(t, issues, "/steps/2/env/Z", "does not provide trigger.pr_numbr", "did you mean trigger.pr_number?")
	mustHave(t, issues, "/steps/2/env/W", "exposes outputs and status", "did you mean steps.aa.outputs?")
	mustHave(t, issues, "/steps/2/env/V", "invalid expression", "")
	mustHave(t, issues, "/steps/2/when", `unknown function "contans"`, "")
	mustHave(t, issues, "/steps/3/input/text", `declares no output "nope"`, "declare it under steps[0].outputs")
	mustHave(t, issues, "/steps/4/command", `declares no output "tss"`, "did you mean steps.dd.outputs.ts?")
	for _, is := range issues {
		if is.Path == "/steps/2/env/U" {
			t.Fatalf("unexpected: %s", is)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, _, err := Parse([]byte("a: [unclosed"), Options{}); err == nil {
		t.Fatal("yaml error expected")
	}
	if _, is, _ := Parse([]byte(""), Options{}); len(is) != 1 {
		t.Fatal("empty document")
	}
	if _, is, _ := Parse([]byte("- a"), Options{}); len(is) != 1 || !strings.Contains(is[0].Message, "mapping") {
		t.Fatal("non-mapping document")
	}
	s := &Spec{Version: 1, Name: "x", Steps: []StepSpec{{ID: "a"}}}
	if is := s.Validate(Options{}); len(is) == 0 {
		t.Fatal("struct-level validation must catch a step with no form")
	}
	if Issues([]Issue{{Path: "/a", Message: "m", Hint: "h"}}).Error() != "/a: m (hint: h)" {
		t.Fatal("issue string")
	}
}

func TestCapture(t *testing.T) {
	doc := map[string]any{"pr": map[string]any{"url": "u"}, "items": []any{map[string]any{"id": 7.0}}}
	cases := map[string]any{"json:pr.url": "u", "json:items.0.id": 7.0, "json:items[0].id": 7.0, "json:$.pr.url": "u"}
	for src, want := range cases {
		c, err := ParseCapture(src)
		if err != nil {
			t.Fatal(src, err)
		}
		got, err := c.FromJSON(doc)
		if err != nil || got != want {
			t.Fatalf("%s = %v %v", src, got, err)
		}
	}
	whole, _ := ParseCapture("json:$")
	if v, _ := whole.FromJSON(doc); !reflect.DeepEqual(v, doc) {
		t.Fatal("whole doc")
	}
	for _, bad := range []string{"json:", "json:a..b", "file:/etc/x", "file:..", "file:", "nope"} {
		if _, err := ParseCapture(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	missing, _ := ParseCapture("json:pr.nope")
	if _, err := missing.FromJSON(doc); err == nil {
		t.Fatal("missing path")
	}
	idx, _ := ParseCapture("json:items.5")
	if _, err := idx.FromJSON(doc); err == nil {
		t.Fatal("bad index")
	}
	scalar, _ := ParseCapture("json:pr.url.x")
	if _, err := scalar.FromJSON(doc); err == nil {
		t.Fatal("scalar descent")
	}
	f, _ := ParseCapture("file:/workspace/out/r.txt")
	if f.File != "out/r.txt" {
		t.Fatal(f.File)
	}
	if LastLine("a\nb\n\n  \n") != "b" || LastLine("") != "" {
		t.Fatal("last line")
	}
	if c, _ := ParseCapture("stdout:last-line"); c.Kind != CaptureStdoutLast {
		t.Fatal("last-line kind")
	}
}
