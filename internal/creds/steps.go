package creds

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// The step half of the catalog (ADR 0037): how an agent CLI runs one
// unattended, one-shot workflow step. It builds on the headless adapter of
// ADR 0035 (the same stream parser) and adds what a step needs: the CLI's
// own tools on, no permission prompts, and typed params.

// Param types.
const (
	ParamString = "string"
	ParamInt    = "int"
	ParamBool   = "bool"
	ParamEnum   = "enum"
)

// Param is one declared knob of a harness: a name the workflow uses, a
// type, and the CLI flag it maps to.
type Param struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Enum []string `json:"enum,omitempty"`
	// Min is the smallest value of an int param.
	Min         int    `json:"min,omitempty"`
	Flag        string `json:"flag"`
	Description string `json:"description,omitempty"`
}

// StepSpec is a harness's step mode.
type StepSpec struct {
	// Params are the declared knobs, in the order flags are emitted.
	Params []Param
	// MinVersion is the oldest CLI version the step mode is modeled on;
	// icaro doctor checks the profile's image against it.
	MinVersion string
	// VersionCommand prints the CLI's version.
	VersionCommand []string
	// base is the unattended one-shot command before params.
	base []string
	// tail goes after the params (e.g. "-" for the prompt on stdin).
	tail []string
}

// ErrNoStep reports an agent without a step mode.
var ErrNoStep = errors.New("this profile's agent cannot run workflow steps (agent steps support claude and codex)")

// Step returns the agent's step mode.
func (a Agent) Step() (*StepSpec, error) {
	if a.step == nil {
		return nil, ErrNoStep
	}
	return a.step, nil
}

// CanStep reports whether the agent has a step mode.
func (a Agent) CanStep() bool { return a.step != nil }

// Param returns the declared param name.
func (s *StepSpec) Param(name string) (Param, bool) {
	for _, p := range s.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// ParamNames lists the declared params, sorted.
func (s *StepSpec) ParamNames() []string {
	out := make([]string, len(s.Params))
	for i, p := range s.Params {
		out[i] = p.Name
	}
	sort.Strings(out)
	return out
}

// CheckParam reports whether v is a valid value for p. Values come from
// YAML, so numbers are float64 or int.
func (p Param) CheckParam(v any) error {
	switch p.Type {
	case ParamString:
		if s, ok := v.(string); !ok || s == "" {
			return errors.New("must be a non-empty string")
		}
	case ParamInt:
		n, ok := asInt(v)
		if !ok {
			return errors.New("must be a whole number")
		}
		if n < p.Min {
			return fmt.Errorf("must be at least %d", p.Min)
		}
	case ParamBool:
		if _, ok := v.(bool); !ok {
			return errors.New("must be true or false")
		}
	case ParamEnum:
		s, _ := v.(string)
		for _, e := range p.Enum {
			if s == e {
				return nil
			}
		}
		return fmt.Errorf("must be one of %v", p.Enum)
	default:
		return fmt.Errorf("unknown param type %q", p.Type)
	}
	return nil
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

// Command returns the step's command for already-validated params. The
// prompt goes in on stdin, never on the command line, where it would be
// size-limited and visible to ps.
func (s *StepSpec) Command(params map[string]any) ([]string, error) {
	cmd := append([]string{}, s.base...)
	for _, p := range s.Params {
		v, ok := params[p.Name]
		if !ok {
			continue
		}
		if err := p.CheckParam(v); err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Name, err)
		}
		switch p.Type {
		case ParamBool:
			if v.(bool) {
				cmd = append(cmd, p.Flag)
			}
		case ParamInt:
			n, _ := asInt(v)
			cmd = append(cmd, p.Flag, strconv.Itoa(n))
		default:
			cmd = append(cmd, p.Flag, v.(string))
		}
	}
	for k := range params {
		if _, ok := s.Param(k); !ok {
			return nil, fmt.Errorf("unknown param %q", k)
		}
	}
	return append(cmd, s.tail...), nil
}

// stepClaude is `claude -p` with stream-json output, its tools on and no
// permission prompts: the container is the confinement. The memory MCP
// server comes from the step's home layer (Configure). Flags are modeled
// on Claude Code's documented headless mode; spike S11 (step mode) pins
// them per CLI version.
var stepClaude = &StepSpec{
	base: []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions"},
	Params: []Param{
		{Name: "model", Type: ParamString, Flag: "--model", Description: "the model alias or name, e.g. sonnet or opus"},
		{Name: "max_turns", Type: ParamInt, Min: 1, Flag: "--max-turns", Description: "stop after this many agent turns"},
	},
	MinVersion:     "2.0.0",
	VersionCommand: []string{"claude", "--version"},
}

// stepCodex is `codex exec --json` with the prompt on stdin ("-"), no
// approvals and no inner sandbox (the container is the sandbox). Codex
// documents no turn limit for exec, so only model is declared until spike
// S11 (step mode) finds one.
var stepCodex = &StepSpec{
	base: []string{"codex", "exec", "--json", "--skip-git-repo-check", "--sandbox", "danger-full-access", "-c", `approval_policy="never"`},
	Params: []Param{
		{Name: "model", Type: ParamString, Flag: "--model", Description: "the model name, e.g. gpt-5-codex"},
	},
	tail:           []string{"-"},
	MinVersion:     "0.40.0",
	VersionCommand: []string{"codex", "--version"},
}
