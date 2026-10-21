package expr

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func env() Env {
	return Env{
		"steps": map[string]any{
			"build-app": map[string]any{"outputs": map[string]any{"url": "http://x", "count": float64(3), "ok": true}},
		},
		"trigger": map[string]any{"pr_number": float64(42), "labels": []any{"bug", "ui"}, "title": "Fix UI", "action": "opened"},
		"env":     map[string]string{"STAGE": "prod"},
		"headers": map[string]any{"x-github-event": "pull_request"},
	}
}

func TestEval(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"steps.build-app.outputs.url", "http://x"},
		{"steps['build-app'].outputs.count == 3", true},
		{"trigger.pr_number > 10 && trigger.action == 'opened'", true},
		{"!(trigger.pr_number < 10) || false", true},
		{"contains(trigger.labels, 'bug')", true},
		{"contains(trigger.title, 'ui')", true},
		{"startsWith(env.STAGE, 'pr')", true},
		{"headers['x-github-event'] == \"pull_request\"", true},
		{"trigger.labels[1]", "ui"},
		{"length(trigger.labels)", float64(2)},
		{"format('{0}#{1}', 'pr', trigger.pr_number)", "pr#42"},
		{"has(trigger.missing)", false},
		{"has(trigger.title) && trigger.title != ''", true},
		{"join(trigger.labels, '+')", "bug+ui"},
		{"fromJSON('{\"a\":[1]}').a[0]", float64(1)},
		{"toJSON(trigger.labels)", `["bug","ui"]`},
		{"coalesce(null, '', 'x')", "x"},
		{"-1 < 0", true},
		{"number('12') >= 12", true},
		{"lower(upper('Ab'))", "ab"},
		{"split('a,b', ',')", []any{"a", "b"}},
		{"null == null", true},
		{"'b' > 'a'", true},
	}
	for _, c := range cases {
		e, err := Parse(c.src)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.src, err)
		}
		got, err := e.Eval(env())
		if err != nil {
			t.Fatalf("%s: eval: %v", c.src, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s = %#v, want %#v", c.src, got, c.want)
		}
	}
}

func TestMissingKeyIsError(t *testing.T) {
	e, _ := Parse("steps.build-app.outputs.nope == 'x'")
	_, err := e.Eval(env())
	var mk *MissingKeyError
	if !errors.As(err, &mk) || mk.Path != "steps.build-app.outputs.nope" {
		t.Fatalf("err = %v", err)
	}
	e, _ = Parse("nothere")
	if _, err := e.Eval(env()); !errors.As(err, &mk) {
		t.Fatalf("root: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{"a ==", "a = b", "foo(1)", "contans('a','b')", "(a", "'x", "a.", "has(1)", "lower()", "a #"} {
		if _, err := Parse(src); err == nil {
			t.Fatalf("%q: expected error", src)
		}
	}
	_, err := Parse("contans('a','b')")
	if !strings.Contains(err.Error(), `did you mean "contains"`) {
		t.Fatalf("hint: %v", err)
	}
}

func TestTypeErrors(t *testing.T) {
	for _, src := range []string{"trigger.title && true", "!trigger.title", "trigger.labels < 1", "trigger.title[0]", "lower(1)"} {
		e, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Eval(env()); err == nil {
			t.Fatalf("%q: expected type error", src)
		}
	}
	e, _ := ParseCondition("trigger.title")
	if _, err := e.EvalBool(env()); err == nil {
		t.Fatal("non-boolean condition must fail")
	}
}

func TestTemplate(t *testing.T) {
	tp, err := ParseTemplate("PR #${{ trigger.pr_number }} at ${{ steps.build-app.outputs.url }} {not}")
	if err != nil {
		t.Fatal(err)
	}
	s, err := tp.Render(env())
	if err != nil || s != "PR #42 at http://x {not}" {
		t.Fatalf("%q %v", s, err)
	}
	whole, _ := ParseTemplate("${{ trigger.labels }}")
	v, _ := whole.Eval(env())
	if !reflect.DeepEqual(v, []any{"bug", "ui"}) {
		t.Fatalf("typed eval: %#v", v)
	}
	mixed, _ := ParseTemplate("x${{ trigger.labels }}")
	v, _ = mixed.Eval(env())
	if v != `x["bug","ui"]` {
		t.Fatalf("mixed: %#v", v)
	}
	if _, err := ParseTemplate("a ${{ b "); err == nil {
		t.Fatal("unterminated")
	}
	if _, err := ParseTemplate("${{ a == '}}' }}"); err != nil {
		t.Fatalf("quoted braces: %v", err)
	}
	lit, _ := ParseTemplate("plain")
	if !lit.IsLiteral() || len(lit.Exprs()) != 0 {
		t.Fatal("literal")
	}
	if s, _ := RenderString("no exprs", nil); s != "no exprs" {
		t.Fatal(s)
	}
}

func TestCondition(t *testing.T) {
	for _, src := range []string{"${{ trigger.pr_number == 42 }}", "trigger.pr_number == 42", "  ${{trigger.pr_number==42}} "} {
		e, err := ParseCondition(src)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := e.EvalBool(env())
		if err != nil || !ok {
			t.Fatalf("%q: %v %v", src, ok, err)
		}
	}
	if _, err := ParseCondition("  "); err == nil {
		t.Fatal("empty")
	}
}

func TestRefs(t *testing.T) {
	e, _ := Parse("steps.a.outputs.x == trigger.labels[steps.b.outputs.i] && has(trigger.y) && contains(env.Z, 'q')")
	var got []string
	for _, r := range e.Refs() {
		s := r.String()
		if r.Partial {
			s += "*"
		}
		if r.Guarded {
			s += "?"
		}
		got = append(got, s)
	}
	want := []string{"steps.a.outputs.x", "steps.b.outputs.i", "trigger.labels*", "trigger.y?", "env.Z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	if err := CheckRoots(e.Refs(), []string{"steps", "trigger", "env"}); err != nil {
		t.Fatal(err)
	}
	err := CheckRoots([]Ref{{Path: []string{"tigger"}}}, []string{"steps", "trigger"})
	if err == nil || !strings.Contains(err.Error(), `did you mean "trigger"`) {
		t.Fatalf("%v", err)
	}
}

func TestEvalValue(t *testing.T) {
	in := map[string]any{"n": "${{ trigger.pr_number }}", "list": []any{"#${{ trigger.pr_number }}", 1}, "keep": true}
	out, err := EvalValue(in, env())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"n": float64(42), "list": []any{"#42", 1}, "keep": true}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("%#v", out)
	}
	var ptrs []string
	WalkTemplates(map[string]any{"a/b": []any{"x"}}, func(p, _ string) { ptrs = append(ptrs, p) })
	if ptrs[0] != "/a~1b/0" {
		t.Fatal(ptrs)
	}
	if _, err := EvalValue(map[string]any{"x": "${{ nope }}"}, env()); err == nil {
		t.Fatal("missing root")
	}
}

func TestStringify(t *testing.T) {
	cases := map[string]any{"": nil, "3": float64(3), "3.5": 3.5, "true": true, `{"a":1}`: map[string]any{"a": 1}}
	for want, v := range cases {
		if got := Stringify(v); got != want {
			t.Fatalf("Stringify(%v) = %q", v, got)
		}
	}
	if Nearest("stpes", []string{"steps", "env"}) != "steps" || Nearest("zzzzzz", []string{"steps"}) != "" {
		t.Fatal("nearest")
	}
	if len(FuncNames()) < 10 {
		t.Fatal("funcs")
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"a.b == 'c'", "has(x.y) && !z", "f(", "${{ a }}", "a[b[c]]",
		"x ${{ 'a\\'}}' }} y", "-1.5 <= a[-2]", "format('{0}', a.b-c)", "!(a || b) && c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := Parse(s)
		if err == nil {
			_, _ = e.Eval(env())
			_ = e.Refs()
		}
		if tp, err := ParseTemplate(s); err == nil {
			_, _ = tp.Render(env())
		}
	})
}
