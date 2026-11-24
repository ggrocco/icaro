package workflow

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeWorkflows is a WorkflowCatalog over parsed YAML documents.
type fakeWorkflows map[string]*Spec

func (f fakeWorkflows) Workflow(name string) (*Spec, bool) { s, ok := f[name]; return s, ok }
func (f fakeWorkflows) WorkflowNames() []string {
	out := make([]string, 0, len(f))
	for n := range f {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func mustSpec(t *testing.T, y string) *Spec {
	t.Helper()
	s, issues, err := Parse([]byte(y), Options{})
	if err != nil || len(issues) > 0 {
		t.Fatalf("%v %v", err, issues)
	}
	return s
}

const calleeYAML = `version: 1
name: run-tests
triggers:
  - type: call
  - type: manual
outputs:
  passed: ${{ steps.test.outputs.passed }}
  report: "${{ steps.test.outputs.report }} for ${{ trigger.repo }}"
steps:
  - id: test
    image: alpine
    command: echo ${{ trigger.repo }} ${{ trigger.sha }} ${{ has(trigger.flags) }}
    outputs: {passed: "json:passed", report: "json:report"}
`

const callerYAML = `version: 1
name: release
triggers: [{type: manual}]
env: {BRANCH: main}
steps:
  - id: checkout
    image: alpine
    outputs: {sha: "stdout"}
  - id: test
    needs: [checkout]
    call:
      workflow: run-tests
      input:
        repo: ${{ trigger.repo }}
        sha: ${{ steps.checkout.outputs.sha }}
    timeout: 2h
    retry: 1
  - id: report
    needs: [test]
    image: alpine
    command: echo ${{ steps.test.outputs.passed }} ${{ steps.test.outputs.report }}
`

func callOpts(t *testing.T, docs ...string) Options {
	t.Helper()
	cat := fakeWorkflows{}
	for _, d := range docs {
		s := mustSpec(t, d)
		cat[s.Name] = s
	}
	return Options{Workflows: cat, MaxTimeout: time.Hour}
}

func TestCallStepValid(t *testing.T) {
	opts := callOpts(t, calleeYAML)
	spec, issues, err := Parse([]byte(callerYAML), opts)
	if err != nil || len(issues) > 0 {
		t.Fatalf("%v %v", err, issues)
	}
	st := spec.Step("test")
	if st.Kind() != KindCall || !st.Pauses() || spec.Calls()[0] != "run-tests" {
		t.Fatal(st.Kind())
	}
	// The call's timeout is a deadline for the child, so limits.timeout
	// does not cap it; its trigger fields count for icaro run.
	req, _ := TriggerFields(spec)
	if strings.Join(req, ",") != "repo" {
		t.Fatal(req)
	}
	callee := opts.Workflows.(fakeWorkflows)["run-tests"]
	if !callee.Callable() || spec.Callable() {
		t.Fatal("callable")
	}
	req, opt := TriggerFields(callee)
	if strings.Join(req, ",") != "repo,sha" || strings.Join(opt, ",") != "flags" {
		t.Fatal(req, opt)
	}
	// Without a workflow catalog (a stored version re-parsed by the
	// engine) the cross-workflow checks are skipped.
	if _, issues, _ := Parse([]byte(callerYAML), Options{}); len(issues) > 0 {
		t.Fatal(issues)
	}
}

func TestCallStepIssues(t *testing.T) {
	opts := callOpts(t, calleeYAML, strings.Replace(strings.Replace(calleeYAML, "name: run-tests", "name: run-lint", 1), "  - type: call\n", "", 1))
	cases := []struct {
		old, new, path, msg, hint string
	}{
		{"workflow: run-tests", "workflow: run-test", "/steps/1/call/workflow", `unknown workflow "run-test"`, `did you mean "run-tests"?`},
		{"workflow: run-tests", "workflow: deploy-prod", "/steps/1/call/workflow", `unknown workflow "deploy-prod"`, "icaro workflow apply"},
		{"workflow: run-tests", "workflow: run-lint", "/steps/1/call/workflow", "workflow run-lint has no call trigger", "- type: call"},
		{"        sha: ${{ steps.checkout.outputs.sha }}\n", "", "/steps/1/call/input", `missing input "sha": workflow run-tests reads trigger.sha`, ""},
		{"        sha:", "        shas:", "/steps/1/call/input/shas", "workflow run-tests does not read trigger.shas", `did you mean "sha"?`},
		{"        sha:", "        branch:", "/steps/1/call/input/branch", "does not read trigger.branch", "it reads: flags, repo, sha"},
		{"repo: ${{ trigger.repo }}", "repo: ${{ secrets.token }}", "/steps/1/call/input/repo", "call input cannot read secrets", "resolves its own"},
		{"repo: ${{ trigger.repo }}", "repo: ${{ steps.report.outputs.x }}", "/steps/1/call/input/repo", "steps.report is not an ancestor of step test", ""},
		{"steps.test.outputs.passed }}", "steps.test.outputs.pass }}", "/steps/2/command", `workflow run-tests declares no output "pass"`, "did you mean steps.test.outputs.passed?"},
		{"steps.test.outputs.passed }}", "steps.test.outputs.coverage }}", "/steps/2/command", `declares no output "coverage"`, "run-tests's top-level outputs"},
		{"    timeout: 2h\n", "    timeout: 2h\n    command: make\n", "/steps/1/command", "command is not valid on call steps", ""},
		{"    timeout: 2h\n", "    timeout: 2h\n    env: {A: b}\n", "/steps/1/env", "env is not valid on call steps", "call.input"},
		{"    timeout: 2h\n", "    timeout: 2h\n    input: {a: b}\n", "/steps/1/input", "input is not valid on call steps", "call.input"},
		{"    timeout: 2h\n", "    timeout: 2h\n    outputs: {a: stdout}\n", "/steps/1/outputs", "outputs is not valid on call steps", "top-level outputs"},
		{"    timeout: 2h\n", "    timeout: 2h\n    repository: {ref: x}\n", "/steps/1/repository", "repository is not valid on call steps", ""},
		{"    call:\n", "    image: alpine\n    call:\n", "/steps/1", "not both image and call", ""},
		{"workflow: run-tests", "workflow: ${{ trigger.wf }}", "/steps/1/call/workflow", "does not match", ""},
	}
	for _, c := range cases {
		if !strings.Contains(callerYAML, c.old) {
			t.Fatalf("case %q: not in the document", c.old)
		}
		_, issues, err := Parse([]byte(strings.Replace(callerYAML, c.old, c.new, 1)), opts)
		if err != nil {
			t.Fatal(err)
		}
		mustHave(t, issues, c.path, c.msg, c.hint)
	}
	// Without the catalog a call step still refuses its forbidden fields.
	_, issues, _ := Parse([]byte(strings.Replace(callerYAML, "    timeout: 2h\n", "    timeout: 2h\n    command: make\n", 1)), Options{})
	mustHave(t, issues, "/steps/1/command", "command is not valid on call steps", "")
}

func TestCallStepReadsNoFields(t *testing.T) {
	plain := "version: 1\nname: tidy-up\ntriggers: [{type: call}]\nsteps:\n  - id: aa\n    image: alpine\n"
	opts := callOpts(t, plain)
	y := "version: 1\nname: nightly\ntriggers: [{type: manual}]\nsteps:\n  - id: c1\n    call: {workflow: tidy-up, input: {x: 1}}\n"
	_, issues, _ := Parse([]byte(y), opts)
	mustHave(t, issues, "/steps/0/call/input/x", "does not read trigger.x", "reads no trigger fields")
}

func TestWorkflowOutputs(t *testing.T) {
	cases := []struct {
		old, new, path, msg, hint string
	}{
		{"${{ steps.test.outputs.passed }}", "${{ steps.tests.outputs.passed }}", "/outputs/passed", "steps.tests: no such step", "did you mean steps.test?"},
		{"${{ steps.test.outputs.passed }}", "${{ steps.test.outputs.pass }}", "/outputs/passed", `step test declares no output "pass"`, ""},
		{"${{ steps.test.outputs.passed }}", "${{ secrets.token }}", "/outputs/passed", "workflow outputs cannot read secrets", ""},
		{"${{ steps.test.outputs.passed }}", "${{ steps.test.outputs.passed", "/outputs/passed", "invalid expression", ""},
		{"  passed:", "  9passed:", "/outputs/9passed", `invalid output name "9passed"`, ""},
	}
	for _, c := range cases {
		_, issues, err := Parse([]byte(strings.Replace(calleeYAML, c.old, c.new, 1)), Options{})
		if err != nil {
			t.Fatal(err)
		}
		mustHave(t, issues, c.path, c.msg, c.hint)
	}
	// A literal output and an output on the built-in result are fine.
	y := strings.Replace(calleeYAML, "  passed: ${{ steps.test.outputs.passed }}", "  passed: \"yes\"", 1)
	if _, issues, _ := Parse([]byte(y), Options{}); len(issues) > 0 {
		t.Fatal(issues)
	}
	// The validator also refuses a bad name the schema let through.
	v := &validator{s: &Spec{Outputs: map[string]string{"9x": "a"}}, index: map[string]int{}}
	v.checkWorkflowOutputs()
	mustHave(t, v.issues, "/outputs/9x", `invalid output name "9x"`, "")
}

func TestCallTrigger(t *testing.T) {
	y := "version: 1\nname: callee\ntriggers: [{type: call, cron: \"* * * * *\"}]\nsteps:\n  - id: aa\n    image: alpine\n"
	_, issues, _ := Parse([]byte(y), Options{})
	mustHave(t, issues, "/triggers/0/cron", "cron is only valid on cron triggers", "")
	_, issues, _ = Parse([]byte(strings.Replace(y, "type: call, cron: \"* * * * *\"", "type: calls", 1)), Options{})
	mustHave(t, issues, "/triggers/0/type", "not one of the allowed values", `did you mean "call"?`)
}

func TestCallCycles(t *testing.T) {
	a := "version: 1\nname: wf-a\ntriggers: [{type: call}]\nsteps:\n  - id: s1\n    call: {workflow: wf-b}\n"
	b := "version: 1\nname: wf-b\ntriggers: [{type: call}]\nsteps:\n  - id: s1\n    call: {workflow: wf-c}\n"
	c := "version: 1\nname: wf-c\ntriggers: [{type: call}]\nsteps:\n  - id: s1\n    image: alpine\n"
	opts := callOpts(t, b, c)
	if _, issues, _ := Parse([]byte(a), opts); len(issues) > 0 {
		t.Fatal(issues)
	}
	// Saving wf-c so that it calls wf-a, with wf-a saved: wf-c → wf-a → wf-b → wf-c.
	opts = callOpts(t, a, b, c)
	c2 := strings.Replace(c, "    image: alpine\n", "    call: {workflow: wf-a}\n  - id: t1\n    call: {workflow: wf-a}\n", 1)
	_, issues, _ := Parse([]byte(c2), opts)
	mustHave(t, issues, "/steps/0/call/workflow", "call cycle: wf-c → wf-a → wf-b → wf-c", "cannot call itself")
	if n := strings.Count(issues.Error(), "call cycle"); n != 1 {
		t.Fatalf("a cycle is reported once: %v", issues)
	}
	// A direct self-call is a cycle even without a catalog.
	self := strings.Replace(c, "    image: alpine\n", "    call: {workflow: wf-c}\n", 1)
	_, issues, _ = Parse([]byte(self), Options{})
	mustHave(t, issues, "/steps/0/call/workflow", "call cycle: wf-c → wf-c", "")
	// A diamond is not a cycle: d calls b twice and c directly.
	d := "version: 1\nname: wf-d\ntriggers: [{type: manual}]\nsteps:\n  - id: s1\n    call: {workflow: wf-b}\n  - id: t1\n    call: {workflow: wf-c}\n  - id: u1\n    call: {workflow: wf-b}\n"
	if _, issues, _ := Parse([]byte(d), callOpts(t, b, c)); len(issues) > 0 {
		t.Fatal(issues)
	}
	// An unknown workflow in the graph ends the walk.
	if _, issues, _ := Parse([]byte(a), callOpts(t, strings.Replace(b, "wf-c", "wf-x", 1))); len(issues) > 0 {
		t.Fatal(issues)
	}
	v := &validator{s: &Spec{}}
	v.checkCallGraph()
	if len(v.issues) != 0 {
		t.Fatal("a nameless spec has no graph")
	}
}

func TestCallerWarnings(t *testing.T) {
	opts := callOpts(t, calleeYAML, callerYAML)
	callee := mustSpec(t, calleeYAML)
	if w := CallerWarnings(callee, opts); len(w) != 0 {
		t.Fatal(w)
	}
	// Dropping an output and requiring a new input breaks the caller.
	edited := strings.Replace(calleeYAML, "  passed: ${{ steps.test.outputs.passed }}\n", "", 1)
	edited = strings.Replace(edited, "${{ has(trigger.flags) }}", "${{ trigger.flags }}", 1)
	w := CallerWarnings(mustSpec(t, edited), opts)
	mustHave(t, w, "/", `caller release would fail validation at /steps/1/call/input: missing input "flags"`, "")
	mustHave(t, w, "/", `caller release would fail validation at /steps/2/command: workflow run-tests declares no output "passed"`, "")
	// A caller's own, older issues are not blamed on the callee.
	broken := strings.Replace(callerYAML, "echo ${{", "echo ${{ env.NOPE }} ${{", 1)
	bs, _, _ := Parse([]byte(broken), Options{})
	opts.Workflows.(fakeWorkflows)["release"] = bs
	if w := CallerWarnings(callee, opts); len(w) != 0 {
		t.Fatal(w)
	}
	if CallerWarnings(callee, Options{}) != nil || CallerWarnings(nil, opts) != nil {
		t.Fatal("no catalog, no warnings")
	}
}
