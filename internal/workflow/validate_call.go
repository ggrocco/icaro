package workflow

import (
	"fmt"
	"sort"
	"strings"

	"icaro/internal/expr"
)

// WorkflowCatalog resolves the workflows call steps name (ADR 0038). The
// workflow service implements it over the current versions; a nil
// catalog skips the cross-workflow checks, as a nil Catalog skips the
// integration ones.
type WorkflowCatalog interface {
	// Workflow returns the current version of a workflow's spec.
	Workflow(name string) (*Spec, bool)
	// WorkflowNames lists every workflow, sorted.
	WorkflowNames() []string
}

// callHint tells an author how to make a workflow callable.
const callHint = "add `- type: call` to its triggers to make it callable"

// checkCall validates a call step's form and, with a workflow catalog, its
// callee, inputs and the call graph through it.
func (v *validator) checkCall(i int, st *StepSpec) {
	base := v.stepPath(i)
	for _, f := range []struct {
		set        bool
		path, hint string
	}{
		{st.Command != "", "command", "the called workflow has its own steps"},
		{len(st.Env) > 0, "env", "pass values to the called workflow through call.input; it reads them as trigger.*"},
		{st.Input != nil, "input", "a call step's input goes under call.input"},
		{len(st.Outputs) > 0, "outputs", "a call step's outputs are the called workflow's outputs; declare them there, under its top-level outputs"},
		{st.Repository != nil, "repository", "the called workflow checks out what it needs in its own steps"},
	} {
		if f.set {
			v.add(base+"/"+f.path, fmt.Sprintf("%s is not valid on call steps", f.path), f.hint)
		}
	}
	name := st.Call.Workflow
	child, ok := v.callee(name)
	if !ok {
		if v.opts.Workflows != nil {
			msg := fmt.Sprintf("unknown workflow %q", name)
			names := v.opts.Workflows.WorkflowNames()
			if h := expr.Nearest(name, names); h != "" {
				v.add(base+"/call/workflow", msg, fmt.Sprintf("did you mean %q?", h))
			} else {
				v.add(base+"/call/workflow", msg, "apply it first (icaro workflow apply); call steps name workflows that exist")
			}
		}
		return
	}
	if !child.Callable() {
		v.add(base+"/call/workflow", fmt.Sprintf("workflow %s has no call trigger, so it cannot be called", name), callHint)
	}
	req, opt := TriggerFields(child)
	reads := append(append([]string{}, req...), opt...)
	sort.Strings(reads)
	for _, k := range sortedKeys(st.Call.Input) {
		if contains(reads, k) {
			continue
		}
		msg := fmt.Sprintf("workflow %s does not read trigger.%s", name, k)
		switch h := expr.Nearest(k, reads); {
		case h != "":
			v.add(base+"/call/input/"+escape(k), msg, fmt.Sprintf("did you mean %q?", h))
		case len(reads) == 0:
			v.add(base+"/call/input/"+escape(k), msg, "it reads no trigger fields; remove input")
		default:
			v.add(base+"/call/input/"+escape(k), msg, "it reads: "+strings.Join(reads, ", "))
		}
	}
	for _, k := range req {
		if _, set := st.Call.Input[k]; !set {
			v.add(base+"/call/input", fmt.Sprintf("missing input %q: workflow %s reads trigger.%s", k, name, k))
		}
	}
}

// callee resolves a called workflow; the spec being validated stands for
// its own name, so a self-call is a cycle, not an unknown workflow.
func (v *validator) callee(name string) (*Spec, bool) {
	if name == v.s.Name {
		return v.s, true
	}
	if v.opts.Workflows == nil {
		return nil, false
	}
	return v.opts.Workflows.Workflow(name)
}

// checkCallGraph refuses a call cycle: the spec's call steps, followed
// through the current versions of the workflows they call, must never
// reach the spec again. Each cycle is reported once, on the first step
// whose call enters it, with its path.
func (v *validator) checkCallGraph() {
	if v.s.Name == "" {
		return
	}
	reported := map[string]bool{}
	for i, st := range v.s.Steps {
		if st.Call == nil || reported[st.Call.Workflow] {
			continue
		}
		reported[st.Call.Workflow] = true
		if path := v.cycleThrough(st.Call.Workflow); path != nil {
			v.add(v.stepPath(i)+"/call/workflow", "call cycle: "+strings.Join(path, " → "),
				"a workflow cannot call itself, directly or through the workflows it calls; move the shared steps into a workflow both can call")
		}
	}
}

// cycleThrough returns the path from the spec through first back to the
// spec, or nil.
func (v *validator) cycleThrough(first string) []string {
	seen := map[string]bool{}
	var walk func(name string, path []string) []string
	walk = func(name string, path []string) []string {
		path = append(path, name)
		if name == v.s.Name {
			return path
		}
		if seen[name] {
			return nil
		}
		seen[name] = true
		spec, ok := v.callee(name)
		if !ok {
			return nil
		}
		for _, next := range spec.Calls() {
			if p := walk(next, path); p != nil {
				return p
			}
		}
		return nil
	}
	return walk(first, []string{v.s.Name})
}

// calledOutputs returns a call step's declared outputs (the callee's
// workflow outputs); ok is false when the callee is not known.
func (v *validator) calledOutputs(st *StepSpec) ([]string, bool) {
	child, ok := v.callee(st.Call.Workflow)
	if !ok || (v.opts.Workflows == nil && child != v.s) {
		return nil, false
	}
	return sortedKeys(child.Outputs), true
}

// checkWorkflowOutputs validates the workflow's outputs: expressions over
// real step outputs, without secrets (ADR 0038).
func (v *validator) checkWorkflowOutputs() {
	for _, k := range sortedKeys(v.s.Outputs) {
		path := "/outputs/" + escape(k)
		val := v.s.Outputs[k]
		if !outputKeyRe.MatchString(k) {
			v.add(path, fmt.Sprintf("invalid output name %q", k), "letters, digits, _ and -, starting with a letter or _")
		}
		if !expr.Contains(val) {
			continue
		}
		t, err := expr.ParseTemplate(val)
		if err != nil {
			v.addExpr(path, "invalid expression: ", err)
			continue
		}
		v.checkCallRefs(path, t.Refs(), -1, "workflow outputs cannot read secrets", "outputs are stored with the run and returned to callers; output a step's result instead")
	}
}

// checkCallRefs is checkRefs for call inputs and workflow outputs: the
// RootsCall names, with a readable issue for secrets.
func (v *validator) checkCallRefs(path string, refs []expr.Ref, step int, msg, hint string) {
	rest := refs[:0:0]
	for _, r := range refs {
		if len(r.Path) > 0 && r.Path[0] == "secrets" {
			v.add(path, msg, hint)
			continue
		}
		rest = append(rest, r)
	}
	v.checkRefs(path, rest, RootsCall, step)
}

// CallerWarnings validates the workflows that call s against s, as it is
// about to be saved: each issue a caller would gain names the caller and
// the place in it. They are warnings, not errors, so a callee's interface
// can change before its callers are updated (ADR 0038); the callers fail
// validation, and their runs, until they are.
func CallerWarnings(s *Spec, opts Options) Issues {
	if s == nil || s.Name == "" || opts.Workflows == nil {
		return nil
	}
	after := opts
	after.Workflows = overlay{WorkflowCatalog: opts.Workflows, spec: s}
	var out Issues
	for _, name := range opts.Workflows.WorkflowNames() {
		if name == s.Name {
			continue
		}
		caller, ok := opts.Workflows.Workflow(name)
		if !ok || !contains(caller.Calls(), s.Name) {
			continue
		}
		before := map[Issue]bool{}
		for _, is := range caller.Validate(opts) {
			before[is] = true
		}
		for _, is := range caller.Validate(after) {
			if before[is] {
				continue
			}
			out = append(out, Issue{Path: "/", Message: fmt.Sprintf("caller %s would fail validation at %s: %s", name, is.Path, is.Message), Hint: is.Hint})
		}
	}
	return out
}

// overlay is a workflow catalog with one workflow replaced.
type overlay struct {
	WorkflowCatalog
	spec *Spec
}

func (o overlay) Workflow(name string) (*Spec, bool) {
	if name == o.spec.Name {
		return o.spec, true
	}
	return o.WorkflowCatalog.Workflow(name)
}
