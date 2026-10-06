package workflow

import "testing"

func TestRenderAndRefs(t *testing.T) {
	data := map[string]any{
		"inputs": map[string]any{"who": "icaro"},
		"steps":  map[string]any{"a": map[string]any{"outputs": map[string]any{"n": 2}}},
	}
	out, err := RenderString("t", `hi {{ inputs.who | upper }} {{ steps.a.outputs.n }} {{ json steps.a.outputs }}`, data)
	if err != nil {
		t.Fatal(err)
	}
	if out != `hi ICARO 2 {"n":2}` {
		t.Fatalf("got %q", out)
	}
	tmpl, _ := ParseTemplate("r", `{{ steps.a.outputs.n }}{{ if steps.b.outputs.x }}{{ steps.c.ok }}{{ end }}`)
	refs := StepRefs(tmpl)
	if len(refs) != 3 || refs[0] != "a" || refs[1] != "b" || refs[2] != "c" {
		t.Fatalf("refs = %v", refs)
	}
}

func TestMissingKeyIsError(t *testing.T) {
	if _, err := RenderString("t", `{{ inputs.nope }}`, map[string]any{"inputs": map[string]any{}}); err == nil {
		t.Fatal("expected missing key error")
	}
}

func TestRenderBool(t *testing.T) {
	data := map[string]any{"inputs": map[string]any{"n": 3}}
	for _, tc := range []struct {
		text string
		want bool
		err  bool
	}{
		{"", true, false},
		{"{{ gt inputs.n 2 }}", true, false},
		{"{{ eq inputs.n 2 }}", false, false},
		{"maybe", false, true},
	} {
		got, err := RenderBool("if", tc.text, data)
		if (err != nil) != tc.err || got != tc.want {
			t.Fatalf("%q: got=%v err=%v", tc.text, got, err)
		}
	}
}
