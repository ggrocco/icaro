package workflow

import (
	"testing"
	"time"
)

const awaitYAML = `
version: 1
name: wait-for-pr
triggers: [{type: manual}]
steps:
  - id: push
    image: alpine:3
    outputs: {branch: "json:branch"}
  - id: pr
    needs: [push]
    await:
      integration: github
      event: pull_request.opened
      key: "${{ steps.push.outputs.branch }}"
      match: event.branch
      filter: "event.repo == 'acme/app'"
    timeout: 48h
  - id: notify
    needs: [pr]
    image: alpine:3
    command: "echo ${{ steps.pr.outputs.pr_number }} ${{ steps.pr.outputs.delivery_id }} ${{ trigger.who }}"
`

// An await step is a pause like approval and wait: it takes needs and
// when, its deadline is not capped by limits.timeout, and its outputs are
// the event's payload fields plus delivery_id.
func TestAwaitStepValid(t *testing.T) {
	s, issues := parseT(t, awaitYAML, Options{MaxTimeout: 30 * time.Minute, Catalog: fakeCatalog{}})
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	pr := s.Step("pr")
	if pr.Kind() != KindAwait || !pr.Pauses() {
		t.Fatal(pr.Kind())
	}
	if pr.Await.Key != "${{ steps.push.outputs.branch }}" || pr.Await.Match != "event.branch" {
		t.Fatalf("%+v", pr.Await)
	}
	if req, _ := TriggerFields(s); len(req) != 1 || req[0] != "who" {
		t.Fatalf("trigger fields: %v", req)
	}
}

// A key that reads the run's trigger is prompted for on manual runs.
func TestAwaitKeyTriggerFields(t *testing.T) {
	s, issues := parseT(t, `version: 1
name: wait-trigger
triggers: [{type: manual}]
steps:
  - id: pr
    await: {integration: github, event: pull_request.opened, key: "${{ trigger.branch }}", match: event.branch}
    timeout: 1h
`, Options{Catalog: fakeCatalog{}})
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	if req, _ := TriggerFields(s); len(req) != 1 || req[0] != "branch" {
		t.Fatalf("trigger fields: %v", req)
	}
}

func TestAwaitStepInvalid(t *testing.T) {
	const ok = "{integration: github, event: pull_request.opened, key: k, match: event.branch}"
	for _, tc := range []struct{ name, step, path, msg, hint string }{
		{"no timeout", "{id: aa, await: " + ok + "}", "/steps/0/timeout", "needs a timeout", "720h"},
		{"long timeout", "{id: aa, await: " + ok + ", timeout: 721h}", "/steps/0/timeout", "maximum await deadline", ""},
		{"with command", "{id: aa, await: " + ok + ", timeout: 1h, command: x}", "/steps/0/command", "not valid on await steps", ""},
		{"with retry", "{id: aa, await: " + ok + ", timeout: 1h, retry: 1}", "/steps/0/retry", "not valid on await steps", ""},
		{"with outputs", "{id: aa, await: " + ok + ", timeout: 1h, outputs: {x: stdout}}", "/steps/0/outputs", "not valid on await steps", ""},
		{"two forms", "{id: aa, wait: 1m, await: " + ok + "}", "/steps/0", "not both wait and await", ""},
		{"unknown integration", "{id: aa, await: {integration: githb, event: push, key: k, match: event.x}, timeout: 1h}", "/steps/0/await/integration", `unknown integration "githb"`, "github"},
		{"unknown event", "{id: aa, await: {integration: github, event: pull_request.opend, key: k, match: event.branch}, timeout: 1h}", "/steps/0/await/event", "has no event", "pull_request.opened"},
		{"unknown match field", "{id: aa, await: {integration: github, event: pull_request.opened, key: k, match: event.brnch}, timeout: 1h}", "/steps/0/await/match", "does not provide event.brnch", "event.branch"},
		{"match reads trigger", "{id: aa, await: {integration: github, event: pull_request.opened, key: k, match: trigger.branch}, timeout: 1h}", "/steps/0/await/match", "trigger", ""},
		{"bad match", "{id: aa, await: {integration: github, event: pull_request.opened, key: k, match: 'event.branch =='}, timeout: 1h}", "/steps/0/await/match", "invalid expression", ""},
		{"unknown filter field", "{id: aa, await: {integration: github, event: pull_request.opened, key: k, match: event.branch, filter: 'event.draft'}, timeout: 1h}", "/steps/0/await/filter", "does not provide event.draft", ""},
		{"secret in key", "{id: aa, await: {integration: github, event: pull_request.opened, key: '${{ secrets.token }}', match: event.branch}, timeout: 1h}", "/steps/0/await/key", "secrets", ""},
		{"empty key", "{id: aa, await: {integration: github, event: pull_request.opened, key: '', match: event.branch}, timeout: 1h}", "/steps/0/await/key", "", ""},
		{"missing match", "{id: aa, await: {integration: github, event: pull_request.opened, key: k}, timeout: 1h}", "/steps/0/await", "match", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, issues := parseT(t, "version: 1\nname: bad-one\nsteps:\n  - "+tc.step+"\n", Options{Catalog: fakeCatalog{}})
			mustHave(t, issues, tc.path, tc.msg, tc.hint)
		})
	}
}

// A key may read only needs ancestors; later steps may read only the
// event's declared fields and delivery_id.
func TestAwaitRefs(t *testing.T) {
	_, issues := parseT(t, `version: 1
name: bad-refs
steps:
  - {id: bb, image: x, outputs: {v: stdout}}
  - id: pr
    await: {integration: github, event: pull_request.opened, key: "${{ steps.bb.outputs.v }}", match: event.branch}
    timeout: 1h
  - {id: xx, needs: [pr], image: x, command: "${{ steps.pr.outputs.prnumber }}"}
`, Options{Catalog: fakeCatalog{}})
	mustHave(t, issues, "/steps/1/await/key", "not an ancestor", `add "bb" to needs`)
	mustHave(t, issues, "/steps/2/command", `declares no output "prnumber"`, "pr_number")
}

// Without a catalog the event and its fields cannot be checked, but the
// shape, the roots and the deadline still are.
func TestAwaitWithoutCatalog(t *testing.T) {
	_, issues := parseT(t, `version: 1
name: no-catalog
steps:
  - id: pr
    await: {integration: anything, event: whatever, key: k, match: event.anything}
    timeout: 1h
  - {id: xx, needs: [pr], image: x, command: "${{ steps.pr.outputs.whatever }}"}
`, Options{})
	if len(issues) > 0 {
		t.Fatal(issues)
	}
}

func TestCorrelator(t *testing.T) {
	c := NewCorrelator()
	ev := map[string]any{"sha": "abc", "n": float64(7), "conclusion": "success"}
	for _, tc := range []struct {
		match, filter, key string
		want               bool
		err                bool
	}{
		{"event.sha", "", "abc", true, false},
		{"${{ event.sha }}", "", "abc", true, false},
		{"event.sha", "", "abd", false, false},
		{"event.n", "", "7", true, false},
		{"event.sha", "event.conclusion == 'success'", "abc", true, false},
		{"event.sha", "event.conclusion != 'success'", "abc", false, false},
		{"event.missing", "", "", false, false},
		{"event.sha", "event.missing == 'x'", "abc", false, false},
		{"event.sha ==", "", "abc", false, true},
		{"event.sha", "event.sha ==", "abc", false, true},
		{"event.sha", "event.sha", "abc", false, true}, // not a boolean
		{"event.sha.x", "", "abc", false, true},        // indexing a string
		{"event.sha", "event.sha.x == 'y'", "abc", false, true},
	} {
		got, err := c.Correlates(tc.match, tc.filter, tc.key, ev)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("%q %q %q: %v %v", tc.match, tc.filter, tc.key, got, err)
		}
	}
	if len(c.cache) == 0 {
		t.Fatal("expressions are cached")
	}
	out := AwaitOutputValues(ev, "d1")
	if out["n"] != "7" || out["sha"] != "abc" || out["delivery_id"] != "d1" || len(out) != 4 {
		t.Fatalf("%v", out)
	}
}

func TestAwaitsAndGenericWebhook(t *testing.T) {
	s, _ := parseT(t, awaitYAML, Options{Catalog: fakeCatalog{}})
	if a := s.Awaits(); len(a) != 1 || a[0].ID != "pr" || s.UsesGenericWebhook() {
		t.Fatalf("%v", a)
	}
	hook := &Spec{Steps: []StepSpec{{ID: "w", Await: &AwaitSpec{Integration: GenericWebhook}}}}
	trig := &Spec{Triggers: []TriggerSpec{{Integration: GenericWebhook}}}
	if !hook.UsesGenericWebhook() || !trig.UsesGenericWebhook() || len(trig.Awaits()) != 0 {
		t.Fatal("generic webhook use")
	}
}
