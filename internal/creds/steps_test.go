package creds

import (
	"errors"
	"strings"
	"testing"
)

func TestStepCommands(t *testing.T) {
	claude, _ := LookupAgent("claude")
	s, err := claude.Step()
	if err != nil || !claude.CanStep() {
		t.Fatal(err)
	}
	cmd, err := s.Command(map[string]any{"model": "sonnet", "max_turns": float64(30)})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmd, " ")
	if !strings.HasPrefix(got, "claude -p --output-format stream-json") || !strings.Contains(got, "--permission-mode bypassPermissions") ||
		!strings.HasSuffix(got, "--model sonnet --max-turns 30") {
		t.Fatal(got)
	}
	if cmd, _ := s.Command(nil); strings.Contains(strings.Join(cmd, " "), "--model") {
		t.Fatal("unset params emitted")
	}
	codex, _ := LookupAgent("codex")
	cs, _ := codex.Step()
	cmd, err = cs.Command(map[string]any{"model": "gpt-5-codex"})
	if err != nil || cmd[len(cmd)-1] != "-" || !strings.Contains(strings.Join(cmd, " "), "--model gpt-5-codex -") {
		t.Fatal(cmd, err)
	}
	if _, err := cs.Command(map[string]any{"max_turns": 3}); err == nil || !strings.Contains(err.Error(), `unknown param "max_turns"`) {
		t.Fatal(err)
	}
	if _, err := s.Command(map[string]any{"max_turns": "ten"}); err == nil {
		t.Fatal("bad type accepted")
	}
	shell, _ := LookupAgent("")
	if _, err := shell.Step(); !errors.Is(err, ErrNoStep) || shell.CanStep() {
		t.Fatal(err)
	}
	if got := s.ParamNames(); strings.Join(got, ",") != "max_turns,model" {
		t.Fatal(got)
	}
	if s.MinVersion == "" || len(s.VersionCommand) == 0 {
		t.Fatal("version")
	}
}

func TestCheckParam(t *testing.T) {
	for _, tc := range []struct {
		p   Param
		v   any
		bad bool
	}{
		{Param{Type: ParamString}, "x", false},
		{Param{Type: ParamString}, "", true},
		{Param{Type: ParamString}, 3, true},
		{Param{Type: ParamInt, Min: 1}, 2, false},
		{Param{Type: ParamInt, Min: 1}, int64(2), false},
		{Param{Type: ParamInt, Min: 1}, float64(2), false},
		{Param{Type: ParamInt, Min: 1}, 2.5, true},
		{Param{Type: ParamInt, Min: 1}, 0, true},
		{Param{Type: ParamInt}, "2", true},
		{Param{Type: ParamBool}, true, false},
		{Param{Type: ParamBool}, "yes", true},
		{Param{Type: ParamEnum, Enum: []string{"a", "b"}}, "b", false},
		{Param{Type: ParamEnum, Enum: []string{"a", "b"}}, "c", true},
		{Param{Type: "weird"}, "x", true},
	} {
		if err := tc.p.CheckParam(tc.v); (err != nil) != tc.bad {
			t.Errorf("%+v %v: %v", tc.p, tc.v, err)
		}
	}
	s := &StepSpec{base: []string{"x"}, Params: []Param{{Name: "fast", Type: ParamBool, Flag: "--fast"}, {Name: "mode", Type: ParamEnum, Enum: []string{"a"}, Flag: "--mode"}}}
	if cmd, _ := s.Command(map[string]any{"fast": true, "mode": "a"}); strings.Join(cmd, " ") != "x --fast --mode a" {
		t.Fatal(cmd)
	}
	if cmd, _ := s.Command(map[string]any{"fast": false}); strings.Join(cmd, " ") != "x" {
		t.Fatal(cmd)
	}
	if _, ok := s.Param("nope"); ok {
		t.Fatal("param")
	}
}
