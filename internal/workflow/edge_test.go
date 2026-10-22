package workflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// TestSemanticIssuesWithoutHints covers the fallback hints used when no
// candidate is close enough to suggest.
func TestSemanticIssuesWithoutHints(t *testing.T) {
	src := `
version: 1
name: hints
env:
  GOOD: x
  1BAD: y
triggers:
  - type: cron
    cron: " "
  - integration: zzzzzz
    event: push
  - integration: github
    event: qqqqqqqqqq
  - integration: github
    event: pull_request.opened
    filter: trigger.zzzzzzzz == 1 && trigger.titel == 'x' && has(trigger.gone)
steps:
  - id: aa
    image: x
    needs: [qqqqqq]
    env: { "1X": y }
    repository: { ref: " ", revison: main }
  - id: bb
    uses: zzzz.yyyy
  - id: cc
    uses: slack.post-message
    input:
      channel: "#c"
      text: t
      wwwwwwww: 1
`
	_, issues := parseT(t, src, Options{Catalog: fakeCatalog{}})
	mustHave(t, issues, "/steps/0/repository/revison", `unknown field "revison"`, `did you mean "revision"?`)

	// Fix the schema issue to reach the semantic checks.
	src = strings.Replace(src, ", revison: main", "", 1)
	_, issues = parseT(t, src, Options{Catalog: fakeCatalog{}})
	mustHave(t, issues, "/env/1BAD", "invalid environment variable name", "")
	mustHave(t, issues, "/triggers/0/cron", "needs a cron expression", "e.g. cron:")
	mustHave(t, issues, "/triggers/1/integration", `unknown integration "zzzzzz"`, "installed: github, slack")
	mustHave(t, issues, "/triggers/2/event", `no event "qqqqqqqqqq"`, "events: pull_request.opened, push")
	mustHave(t, issues, "/triggers/3/filter", "does not provide trigger.zzzzzzzz", "fields: pr_number")
	mustHave(t, issues, "/triggers/3/filter", "does not provide trigger.titel", "did you mean trigger.title?")
	mustHave(t, issues, "/steps/0/needs/0", `unknown step "qqqqqq"`, "known steps: aa, bb, cc")
	mustHave(t, issues, "/steps/0/env/1X", "invalid environment variable name", "")
	mustHave(t, issues, "/steps/0/repository/ref", "repository ref is empty", "")
	mustHave(t, issues, "/steps/1/uses", `unknown action "zzzz.yyyy"`, "install the integration pack")
	mustHave(t, issues, "/steps/2/input/wwwwwwww", `unknown input "wwwwwwww"`, "inputs: blocks, channel, text")
	for _, is := range issues {
		if strings.Contains(is.Message, "trigger.gone") {
			t.Fatalf("guarded field reported: %s", is)
		}
	}
}

func TestFilterExpressionErrors(t *testing.T) {
	src := `
version: 1
name: filters
triggers:
  - integration: github
    event: pull_request.opened
    filter: "trigger.title =="
  - integration: github
    event: pull_request.opened
    filter: "steps.a == 1"
steps:
  - { id: aa, image: x }
`
	_, issues := parseT(t, src, Options{})
	mustHave(t, issues, "/triggers/0/filter", "invalid expression: unexpected end of expression", "")
	mustHave(t, issues, "/triggers/1/filter", `unknown name "steps"`, "")
}

func TestReferenceIssuesWithoutHints(t *testing.T) {
	src := `
version: 1
name: refs
env:
  E: x
triggers:
  - integration: github
    event: pull_request.opened
  - integration: github
    event: push
steps:
  - id: aa
    image: x
    outputs: { result: stdout }
  - id: bb
    uses: slack.post-message
    input: { channel: c, text: t }
  - id: cc
    image: x
  - id: dd
    uses: zzzz.yyyy
  - id: ee
    image: x
    needs: [aa, bb, cc, dd]
    command: >-
      ${{ trigger }} ${{ steps.aa }} ${{ steps.aa.outputs }}
      ${{ has(steps.aa.outputs.maybe) }} ${{ steps.aa.status }}
      ${{ steps.qqqqqqqq.outputs.x }} ${{ steps.aa.zzzzzzzz }}
      ${{ steps.bb.outputs.zzzzzzzz }} ${{ steps.dd.outputs.any }}
      ${{ env.zzzzzzzz }} ${{ has(env.optional) }} ${{ trigger.anything }}
      ${{ has(secrets.x) }}
`
	_, issues := parseT(t, src, Options{Catalog: fakeCatalog{}, SecretExists: func(string) bool { return false }})
	path := "/steps/4/command"
	mustHave(t, issues, path, "steps.qqqqqqqq: no such step", "")
	mustHave(t, issues, path, "steps.aa.zzzzzzzz: a step exposes outputs and status", "")
	mustHave(t, issues, path, `step bb declares no output "zzzzzzzz"`, "outputs: ts, channel")
	mustHave(t, issues, path, "env.zzzzzzzz is not a workflow env variable", "declare it under the top-level env")
	// trigger.anything is unchecked: github/push is unknown to the catalog,
	// even though it is listed after a known event (regression: the known
	// event was checked first and reported).
	for _, is := range issues {
		if is.Path != path {
			continue
		}
		for _, bad := range []string{"maybe", "steps.dd", "optional", "trigger.anything", "secret"} {
			if strings.Contains(is.Message, bad) {
				t.Fatalf("unexpected issue: %s", is)
			}
		}
	}

	// Without a catalog, outputs of uses steps are not checked.
	_, issues = parseT(t, src, Options{})
	for _, is := range issues {
		if strings.Contains(is.Message, "step bb") {
			t.Fatalf("unexpected issue without catalog: %s", is)
		}
	}
}

// TestStructValidation exercises rules the schema already enforces on
// YAML input but that Validate must enforce for specs built in code.
func TestStructValidation(t *testing.T) {
	s := &Spec{Version: 2, Name: "x", Steps: []StepSpec{
		{ID: "aa", Image: "x", Timeout: "0s", Retry: 9},
		{ID: "bb", Image: "x", Timeout: "soon", Retry: -1},
		{ID: "cc", Uses: "slack.post-message"},
		{ID: "dd", Image: "x", Needs: []string{"ff"}, Command: "${{ steps.ff.outputs.x }}"},
		{ID: "ee", Image: "x", Needs: []string{"aa"}, Command: "${{ steps.zz.outputs.x }}"},
		{ID: "ff", Needs: []string{"aa"}},
		{ID: "gg", Image: "x", Needs: []string{"ff"}, Command: "${{ steps.ff.outputs.x }}"},
	}}
	issues := s.Validate(Options{})
	mustHave(t, issues, "/version", "unsupported version 2", "use version: 1")
	mustHave(t, issues, "/steps/0/timeout", "must be a positive duration", "")
	mustHave(t, issues, "/steps/0/retry", "between 0 and 5", "")
	mustHave(t, issues, "/steps/1/timeout", "must be a positive duration", "")
	mustHave(t, issues, "/steps/1/retry", "between 0 and 5", "")
	mustHave(t, issues, "/steps/5", "exactly one of: image, uses", "")
	// A uses step without a catalog is not resolved, and a step with no
	// form declares nothing to check against.
	for _, is := range issues {
		if strings.HasPrefix(is.Path, "/steps/2") || strings.HasPrefix(is.Path, "/steps/6") {
			t.Fatalf("unexpected issue: %s", is)
		}
	}
}

func TestCheckInputType(t *testing.T) {
	cases := []struct {
		typ string
		val any
		ok  bool
	}{
		{"", 1, true},
		{"any", "x", true},
		{"string", "x", true},
		{"string", 1.0, false},
		{"number", 1.5, true},
		{"integer", 3, true},
		{"number", "3", false},
		{"boolean", true, true},
		{"boolean", "true", false},
		{"object", map[string]any{}, true},
		{"object", []any{}, false},
		{"array", []any{}, true},
		{"array", "${{ steps.a.outputs.list }}", true},
		{"unknown-type", 1, true},
	}
	for _, c := range cases {
		err := checkInputType(c.typ, c.val)
		if (err == nil) != c.ok {
			t.Fatalf("checkInputType(%q, %#v) = %v, want ok=%v", c.typ, c.val, err, c.ok)
		}
	}
}

func TestTriggerFieldsOptionalAndRequired(t *testing.T) {
	s := &Spec{
		Env: map[string]string{"A": "${{ has(trigger.label) }}"},
		Steps: []StepSpec{
			{ID: "a", Image: "x", Command: "${{ trigger.number }}", Env: map[string]string{"B": "${{ has(trigger.number) }}"}},
			{ID: "b", Uses: "x.y", Input: map[string]any{"t": "${{ trigger.title }}", "n": []any{"${{ secrets.s1 }}"}}, When: "has(trigger.draft) && trigger.ok"},
			{ID: "c", Image: "x", When: "(", Env: map[string]string{"S": "${{ secrets.s2 }}"}},
		},
	}
	req, opt := TriggerFields(s)
	if !reflect.DeepEqual(req, []string{"number", "ok", "title"}) || !reflect.DeepEqual(opt, []string{"draft", "label"}) {
		t.Fatalf("required %v optional %v", req, opt)
	}
	if got := SecretRefs(s); !reflect.DeepEqual(got, []string{"s1", "s2"}) {
		t.Fatalf("secrets %v", got)
	}
}

func TestSpecHelpers(t *testing.T) {
	s := &Spec{Steps: []StepSpec{{ID: "a"}, {ID: "b"}}}
	if s.Step("b") != &s.Steps[1] || s.Step("z") != nil {
		t.Fatal("Step lookup")
	}
	if s.ConcurrencyPolicy() != ConcurrencyAllow {
		t.Fatal("default policy is allow")
	}
	if (&StepSpec{Image: "x", Uses: "y"}).Kind() != "" {
		t.Fatal("both forms have no kind")
	}
	if (Issue{Path: "/a", Message: "m"}).String() != "/a: m" {
		t.Fatal("issue without hint")
	}
	if _, err := NewPlan(&Spec{Steps: []StepSpec{{ID: "a", Needs: []string{"z"}}}}); err == nil || !strings.Contains(err.Error(), "unknown step z") {
		t.Fatalf("NewPlan: %v", err)
	}
}

func TestDecodeEdgeCases(t *testing.T) {
	// A value JSON cannot encode is an error, not an issue.
	if _, _, err := Parse([]byte("version: 1\nname: abc\nsteps: [{id: aa, image: x, input: {n: .inf}}]\n"), Options{}); err == nil {
		t.Fatal("infinite number must fail encoding")
	}
	// A type mismatch caught by the schema is reported once, without the
	// decoder's error.
	_, issues, err := Parse([]byte("version: 1\nname: abc\nsteps: [{id: aa, image: x, retry: \"2\"}]\n"), Options{})
	if err != nil || len(issues) == 0 {
		t.Fatalf("issues %v err %v", issues, err)
	}
	for _, is := range issues {
		if strings.Contains(is.Message, "cannot unmarshal") {
			t.Fatalf("decoder error leaked: %s", is)
		}
	}
}

func TestNormalize(t *testing.T) {
	in := map[any]any{1: int64(2), "u": uint64(3), "l": []any{4}, "m": map[string]any{"x": 5}}
	want := map[string]any{"1": 2.0, "u": 3.0, "l": []any{4.0}, "m": map[string]any{"x": 5.0}}
	if got := normalize(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalize = %#v", got)
	}
}

func TestValueAt(t *testing.T) {
	doc := map[string]any{"a": []any{"x", map[string]any{"b": 1}}}
	cases := []struct {
		loc  []string
		want any
	}{
		{[]string{"a", "1", "b"}, 1},
		{[]string{"a", "x"}, nil},
		{[]string{"a", "9"}, nil},
		{[]string{"a", "-1"}, nil},
		{[]string{"a", "0", "deeper"}, nil},
	}
	for _, c := range cases {
		if got := valueAt(doc, c.loc); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("valueAt(%v) = %#v", c.loc, got)
		}
	}
}

func TestKnownKeysAt(t *testing.T) {
	doc := map[string]any{"triggers": []any{map[string]any{"type": "manual"}, map[string]any{"event": "x"}}}
	cases := []struct {
		loc  []string
		want []string
	}{
		{[]string{"steps", "0", "repository"}, []string{"ref", "revision"}},
		{[]string{"triggers", "0"}, []string{"type", "cron"}},
		{[]string{"triggers", "1"}, []string{"integration", "event", "filter"}},
		{[]string{"nope"}, nil},
		{[]string{"name", "x"}, nil},
		{[]string{"env"}, nil},
	}
	for _, c := range cases {
		if got := knownKeysAt(c.loc, doc); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("knownKeysAt(%v) = %v, want %v", c.loc, got, c.want)
		}
	}
}

func TestOneOfFallbacks(t *testing.T) {
	doc := map[string]any{"triggers": []any{map[string]any{"type": "manual"}}}
	// A trigger variant whose causes yield nothing still gets an issue.
	e := &jsonschema.ValidationError{InstanceLocation: []string{"triggers", "0"}, ErrorKind: &kind.OneOf{}}
	if got := oneOfIssues(e, "/triggers/0", doc); len(got) != 1 || got[0].Message != "invalid trigger" {
		t.Fatalf("trigger fallback: %v", got)
	}
	// oneOf elsewhere reports every cause.
	cause := &jsonschema.ValidationError{InstanceLocation: []string{"x"}, ErrorKind: &kind.Required{Missing: []string{"y"}}}
	e = &jsonschema.ValidationError{InstanceLocation: []string{"x"}, ErrorKind: &kind.OneOf{}, Causes: []*jsonschema.ValidationError{cause}}
	if got := oneOfIssues(e, "/x", doc); len(got) != 1 || !strings.Contains(got[0].Message, "missing required field(s): y") {
		t.Fatalf("generic oneOf: %v", got)
	}
}

func TestSchemaGeneratorGuards(t *testing.T) {
	type tagged struct {
		Plain   string
		Skipped string `json:"-"`
		Named   string `json:"named,omitempty"`
	}
	typ := reflect.TypeOf(tagged{})
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, jsonName(typ.Field(i)))
	}
	if !reflect.DeepEqual(names, []string{"Plain", "", "named"}) {
		t.Fatalf("jsonName = %v", names)
	}
	if _, ok := fieldByJSONName(typ, "absent"); ok {
		t.Fatal("absent field found")
	}

	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: expected a panic", name)
			}
		}()
		fn()
	}
	// Adding a spec field without an annotation, or with a malformed one,
	// or of a type the generator cannot express, fails loudly.
	mustPanic("missing annotation", func() { propertySchema("Nope.field", reflect.TypeOf("")) })
	fieldAnnotations["Test.bad"] = "{"
	defer delete(fieldAnnotations, "Test.bad")
	mustPanic("bad annotation", func() { propertySchema("Test.bad", reflect.TypeOf("")) })
	mustPanic("unsupported type", func() { typeSchema(reflect.TypeOf(1.5)) })

	if got := typeSchema(reflect.TypeOf(map[string]bool{})); !reflect.DeepEqual(got["additionalProperties"], map[string]any{"type": "boolean"}) {
		t.Fatalf("typed map: %v", got)
	}
}
