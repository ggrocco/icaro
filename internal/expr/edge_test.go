package expr

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestEvalEdges covers operators, functions and indexing beyond the happy
// paths in TestEval.
func TestEvalEdges(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"false && nope", false},
		{"true || nope", true},
		{"false || true", true},
		{"1 <= 1 && 2 > 1 && 2 >= 2 && 1 != 2", true},
		{"'a' <= 'a'", true},
		{"contains(trigger.labels, 'none')", false},
		{"length('héllo')", float64(5)},
		{"length(steps)", float64(1)},
		{"join(trigger.labels)", "bug,ui"},
		{"join(split('a-b', '-'), '')", "ab"},
		{"coalesce(null, '')", nil},
		{"string(3)", "3"},
		{"number(4)", float64(4)},
		{"number(' 2.5 ')", 2.5},
		{"trim('  x ')", "x"},
		{"endsWith('abc', 'bc')", true},
		{"format('none')", "none"},
		{"env['STAGE']", "prod"},
		{"trigger.labels[0]", "bug"},
		{"has(steps.build-app.outputs.url)", true},
		{"has(fromJSON('null').x)", false},
		{"fromJSON('{\"a\":{\"b\":1}}')['a'].b == 1", true},
		{"(1)", float64(1)},
		{"!\n\tfalse", true},
		{"'a\\'b\\n\\t\\\\'", "a'b\n\t\\"},
	}
	for _, c := range cases {
		e, err := Parse(c.src)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.src, err)
		}
		if e.String() != c.src {
			t.Fatalf("String() = %q", e.String())
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

func TestEvalErrors(t *testing.T) {
	cases := []struct {
		src     string
		msg     string // substring of the error
		missing string // expected MissingKeyError path, if any
	}{
		{src: "nope.x", missing: "nope"},
		{src: "trigger[nope]", missing: "nope"},
		{src: "trigger[1]", msg: "cannot index an object with a number"},
		{src: "env[true]", msg: "cannot index an object with a boolean"},
		{src: "env.NOPE", missing: "env.NOPE"},
		{src: "trigger.labels[1.5]", msg: "cannot index an array with a number"},
		{src: "trigger.labels['a']", msg: "cannot index an array with a string"},
		{src: "trigger.labels[-1]", missing: "trigger.labels.-1"},
		{src: "trigger.labels[length(trigger.labels)]", missing: "trigger.labels[...]"},
		{src: "fromJSON('null').x", missing: "(value).x"},
		{src: "trigger.pr_number.x", msg: "cannot index a number (at trigger.pr_number.x)"},
		{src: "has(trigger.title[0])", msg: "cannot index a string"},
		{src: "!nope", missing: "nope"},
		{src: "!1", msg: "! needs a boolean, got a number"},
		{src: "!trigger", msg: "got an object"},
		{src: "!trigger.labels", msg: "got an array"},
		{src: "!null", msg: "got null"},
		{src: "!'x'", msg: "got a string"},
		{src: "nope && true", missing: "nope"},
		{src: "true && nope", missing: "nope"},
		{src: "true && 'x'", msg: "&& needs booleans, got a string"},
		{src: "false || 1", msg: "|| needs booleans, got a number"},
		{src: "nope == 1", missing: "nope"},
		{src: "1 == nope", missing: "nope"},
		{src: "1 < 'a'", msg: "cannot order a number and a string"},
		{src: "lower(nope)", missing: "nope"},
		{src: "startsWith(1, 'a')", msg: "startsWith(): want a string, got a number"},
		{src: "endsWith('a', 1)", msg: "want a string, got a number"},
		{src: "contains('a', 1)", msg: "want a string, got a number"},
		{src: "contains(1, 'a')", msg: "want a string or array, got a number"},
		{src: "length(true)", msg: "want a string, array or object, got a boolean"},
		{src: "fromJSON(1)", msg: "want a string"},
		{src: "fromJSON('{')", msg: "fromJSON()"},
		{src: "join('a')", msg: "want an array, got a string"},
		{src: "join(trigger.labels, 1)", msg: "want a string"},
		{src: "format(1)", msg: "want a string"},
		{src: "number(true)", msg: "want a string, got a boolean"},
		{src: "number('x')", msg: "invalid syntax"},
		{src: "upper(1)", msg: "want a string"},
		{src: "trim(1)", msg: "want a string"},
		{src: "split(1, ',')", msg: "want a string"},
	}
	for _, c := range cases {
		e, err := Parse(c.src)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.src, err)
		}
		_, err = e.Eval(env())
		if err == nil {
			t.Fatalf("%s: expected an error", c.src)
		}
		if c.missing != "" {
			var mk *MissingKeyError
			if !errors.As(err, &mk) || mk.Path != c.missing {
				t.Fatalf("%s: err = %v, want missing %q", c.src, err, c.missing)
			}
			continue
		}
		if !strings.Contains(err.Error(), c.msg) {
			t.Fatalf("%s: err = %v, want %q", c.src, err, c.msg)
		}
	}
}

func TestParseErrorMessages(t *testing.T) {
	cases := []struct{ src, msg string }{
		{"a ==", "unexpected end of expression"},
		{"a 'x'", `unexpected "x"`},
		{"a b", `unexpected "b"`},
		{"a-", "unexpected"},
		{"a -1", `unexpected character '-'`},
		{"(1) -1", `unexpected character '-'`},
		{"1.2.3", `invalid number "1.2.3"`},
		{"'abc", "unterminated string"},
		{"a = 1", "use == for comparison"},
		{"a ~ b", `unexpected character '~'`},
		{"true || )", `unexpected ")"`},
		{"true && )", `unexpected ")"`},
		{"1 == )", `unexpected ")"`},
		{"!)", `unexpected ")"`},
		{"a[)", `unexpected ")"`},
		{"a[1", `expected "]"`},
		{"a.1", "expected a name after '.'"},
		{"has(a.b, c.d)", "has() takes one reference"},
		{"has(a)", "has() takes a reference"},
		{"lower(1, 2)", "lower() takes 1 argument(s), got 2"},
		{"format()", "format() takes at least 1 argument(s), got 0"},
		{"join()", "join() takes 1-2 argument(s), got 0"},
		{"lower(1 2)", `expected ","`},
		{"lower(", "unexpected end of expression"},
		{"zzzzzzzz(1)", `unknown function "zzzzzzzz"`},
		{")", `unexpected ")"`},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil {
			t.Fatalf("%q: expected an error", c.src)
		}
		if !strings.Contains(err.Error(), c.msg) {
			t.Fatalf("%q: err = %v, want %q", c.src, err, c.msg)
		}
		var pe *Error
		if !errors.As(err, &pe) || pe.Src != c.src {
			t.Fatalf("%q: want *Error with the source, got %#v", c.src, err)
		}
	}
	if _, err := Parse("zzzzzzzz(1)"); strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("no hint expected: %v", err)
	}
	if got := (&Error{Pos: -1, Msg: "plain"}).Error(); got != "plain" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestEvalBoolError(t *testing.T) {
	e, _ := ParseCondition("nope")
	if _, err := e.EvalBool(env()); err == nil {
		t.Fatal("eval error must propagate")
	}
}

func TestRefsEdges(t *testing.T) {
	cases := []struct {
		src  string
		want []string
	}{
		{"1", nil},
		{"trigger", []string{"trigger"}},
		{"trigger.labels[steps.i].name", []string{"steps.i", "trigger.labels*"}},
		{"trigger['a'][0]", []string{"trigger.a.0"}},
		{"fromJSON(trigger.body).a[steps.k]", []string{"trigger.body", "steps.k"}},
		{"has(trigger.x) || !steps.x", []string{"trigger.x?", "steps.x"}},
	}
	for _, c := range cases {
		e, err := Parse(c.src)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
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
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: refs = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestTemplateEdges(t *testing.T) {
	tp, err := ParseTemplate("a ${{ steps.x }} b ${{ trigger.y }}")
	if err != nil {
		t.Fatal(err)
	}
	if tp.IsLiteral() || len(tp.Exprs()) != 2 {
		t.Fatal("two expressions expected")
	}
	if got := len(tp.Refs()); got != 2 {
		t.Fatalf("refs = %d", got)
	}
	if _, err := tp.Render(env()); err == nil {
		t.Fatal("missing key must fail Render")
	}

	// Parse errors report the offset in the whole template, counting the
	// whitespace after ${{ (regression: it was dropped).
	_, err = ParseTemplate("abc ${{ a = b }}")
	var pe *Error
	if !errors.As(err, &pe) || pe.Src != "abc ${{ a = b }}" || pe.Pos != 10 {
		t.Fatalf("err = %#v", err)
	}

	// An escaped quote inside a string does not end it.
	tp, err = ParseTemplate(`${{ 'a\'}}' }}`)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := tp.Eval(env()); v != "a'}}" {
		t.Fatalf("v = %#v", v)
	}

	if s, err := RenderString("#${{ trigger.pr_number }}", env()); err != nil || s != "#42" {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := RenderString("${{ a = }}", env()); err == nil {
		t.Fatal("parse error expected")
	}
}

func TestEvalValueErrors(t *testing.T) {
	if _, err := EvalValue("${{ a =", env()); err == nil {
		t.Fatal("parse error expected")
	}
	_, err := EvalValue([]any{"ok", "${{ nope }}"}, env())
	if err == nil || !strings.HasPrefix(err.Error(), "[1]: ") {
		t.Fatalf("err = %v", err)
	}
	if v, _ := EvalValue("plain", env()); v != "plain" {
		t.Fatal(v)
	}
}

func TestNumbersAndEquality(t *testing.T) {
	nums := []any{float32(2), 2, int64(2), int32(2), json.Number("2")}
	for _, n := range nums {
		if !Equal(n, 2.0) {
			t.Fatalf("Equal(%T, 2.0) = false", n)
		}
		if got := typeName(n); got != "a number" {
			t.Fatalf("typeName(%T) = %q", n, got)
		}
	}
	if _, ok := toNumber(json.Number("x")); ok {
		t.Fatal("invalid json.Number must not convert")
	}
	if Equal(1, "1") {
		t.Fatal("number and string differ")
	}
	if !Equal(map[string]string{"a": "b"}, map[string]any{"a": "b"}) {
		t.Fatal("string maps normalize to objects")
	}
	if !Equal(map[string]any{"l": []any{1, int64(2)}}, map[string]any{"l": []any{1.0, 2.0}}) {
		t.Fatal("nested numbers normalize")
	}
	if typeName(true) != "a boolean" || typeName(map[string]string{}) != "an object" || typeName(struct{}{}) != "struct {}" {
		t.Fatal("typeName")
	}
}

func TestStringifyEdges(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{false, "false"},
		{7, "7"},
		{1e20, "100000000000000000000"},
		{-0.25, "-0.25"},
		{[]any{"a"}, `["a"]`},
	}
	for _, c := range cases {
		if got := Stringify(c.v); got != c.want {
			t.Fatalf("Stringify(%v) = %q, want %q", c.v, got, c.want)
		}
	}
	ch := make(chan int)
	if got := Stringify(ch); !strings.HasPrefix(got, "0x") {
		t.Fatalf("unmarshalable falls back to fmt: %q", got)
	}
	if _, err := funcs["toJSON"].call([]any{ch}); err == nil {
		t.Fatal("toJSON of a channel must fail")
	}
}

func TestNearestEdges(t *testing.T) {
	if Nearest("x", nil) != "" {
		t.Fatal("no candidates")
	}
	if Nearest("steps", []string{"steps"}) != "" {
		t.Fatal("exact match is not a typo")
	}
	if Nearest("integratoins", []string{"integrations"}) != "integrations" {
		t.Fatal("long names allow three edits")
	}
	if Nearest("abcd", []string{"wxyz"}) != "" {
		t.Fatal("far candidates are not suggested")
	}
	if err := CheckRoots([]Ref{{Path: []string{"zzzzzz"}}, {}}, []string{"steps"}); err == nil || strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("err = %v", err)
	}
}
