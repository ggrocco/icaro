package workflow

import (
	"strings"
	"testing"
)

var agentOpts = Options{Agents: &AgentOptions{Profiles: []AgentProfile{{Name: "reviewer", Agent: "claude"}, {Name: "coder", Agent: "codex"}}}}

const agentYAML = `version: 1
name: review
triggers: [{type: manual}]
env: {BRANCH: main}
steps:
  - id: diff
    image: alpine
    outputs: {files: "stdout"}
  - id: review
    needs: [diff]
    agent:
      prompt: "Review ${{ steps.diff.outputs.files }} on ${{ env.BRANCH }} for run ${{ run.id }}"
      params: {model: sonnet, max_turns: 30}
    outputs: {summary: "json:summary"}
  - id: post
    needs: [review]
    image: alpine
    command: echo "${{ steps.review.outputs.result }} ${{ steps.review.outputs.summary }}"
`

func TestAgentStepValid(t *testing.T) {
	spec, issues, err := Parse([]byte(agentYAML), agentOpts)
	if err != nil || len(issues) > 0 {
		t.Fatalf("%v %v", err, issues)
	}
	st := spec.Steps[1]
	if st.Kind() != KindAgent || !st.Agent.MemoryOn() {
		t.Fatal(st.Kind())
	}
	if p, ok := agentOpts.Agents.AgentStepProfile(st.Agent); !ok || p.Name != "reviewer" {
		t.Fatalf("default profile %+v", p)
	}
	off := false
	st.Agent.Memory = &off
	if st.Agent.MemoryOn() {
		t.Fatal("memory off")
	}
	// Without agent options (a stored version re-parsed by the engine) the
	// profile checks are skipped.
	if _, issues, _ := Parse([]byte(agentYAML), Options{}); len(issues) > 0 {
		t.Fatal(issues)
	}
	if len(Warnings(spec, agentOpts)) != 0 {
		t.Fatal("a manual workflow has no untrusted input")
	}
}

func TestAgentStepOffAndUnlisted(t *testing.T) {
	_, issues, _ := Parse([]byte(agentYAML), Options{Agents: &AgentOptions{}})
	mustHave(t, issues, "/steps/1/agent", "agent steps are off: no profile is listed in workflows.agent_profiles", "workflows.agent_profiles")
	y := strings.Replace(agentYAML, "      prompt:", "      profile: reviewers\n      prompt:", 1)
	_, issues, _ = Parse([]byte(y), agentOpts)
	mustHave(t, issues, "/steps/1/agent/profile", `profile "reviewers" is not listed in workflows.agent_profiles`, `did you mean "reviewer"?`)
	y = strings.Replace(agentYAML, "      prompt:", "      profile: zzzzzzzz\n      prompt:", 1)
	_, issues, _ = Parse([]byte(y), agentOpts)
	mustHave(t, issues, "/steps/1/agent/profile", "is not listed", "listed: reviewer, coder")
	// A listed profile whose agent cannot run steps (config validation
	// stops this; the validator still says why).
	_, issues, _ = Parse([]byte(agentYAML), Options{Agents: &AgentOptions{Profiles: []AgentProfile{{Name: "sh", Agent: ""}}}})
	mustHave(t, issues, "/steps/1/agent", "cannot run workflow steps", "")
	_, issues, _ = Parse([]byte(agentYAML), Options{Agents: &AgentOptions{Profiles: []AgentProfile{{Name: "x", Agent: "vim"}}}})
	mustHave(t, issues, "/steps/1/agent", `unknown agent "vim"`, "")
}

func TestAgentStepInvalid(t *testing.T) {
	y := `version: 1
name: bad-agent
triggers: [{type: manual}]
steps:
  - id: early
    image: alpine
  - id: one
    agent:
      profile: coder
      prompt: "use ${{ secrets.TOKEN }} and ${{ steps.early.outputs.x }} ${{ steps.later.outputs.result }}"
      params: {models: gpt-5, }
    command: echo hi
    outputs: {result: "stdout", "bad name": "stdout"}
  - id: two
    agent:
      prompt: "x"
      params: {max_turns: "lots", model: ""}
  - id: three
    image: alpine
  - id: four
    agent: {prompt: "  "}
  - id: later
    needs: [one]
    agent: {prompt: "${{ steps.one.outputs.nope }}"}
`
	_, issues, _ := Parse([]byte(y), agentOpts)
	mustHave(t, issues, "/steps/1/command", "command is not valid on agent steps", "harness supplies the command")
	mustHave(t, issues, "/steps/1/agent/prompt", "the prompt cannot read secrets", "env")
	mustHave(t, issues, "/steps/1/agent/prompt", "steps.early is not an ancestor of step one", `add "early" to needs`)
	mustHave(t, issues, "/steps/1/agent/params/models", `codex has no param "models"`, `did you mean "model"?`)
	mustHave(t, issues, "/steps/1/outputs/result", "built-in output", "")
	mustHave(t, issues, "/steps/1/outputs/bad name", "invalid output name", "")
	mustHave(t, issues, "/steps/2/agent/params/max_turns", "must be a whole number", "agent turns")
	mustHave(t, issues, "/steps/2/agent/params/model", "non-empty string", "")
	mustHave(t, issues, "/steps/5/agent/prompt", `step one declares no output "nope"`, "declare it under steps[1].outputs")
	_, issues, _ = Parse([]byte(strings.Replace(y, "agent: {prompt: \"  \"}", "agent: {prompt: \"  \", params: {zzzz: 1}}", 1)), agentOpts)
	mustHave(t, issues, "/steps/4/agent/prompt", "needs a prompt", "")
	mustHave(t, issues, "/steps/4/agent/params/zzzz", `claude has no param "zzzz"`, "params: max_turns, model")
	// The schema refuses image on an agent step, unknown agent fields and
	// two forms at once.
	_, issues, _ = Parse([]byte("version: 1\nname: with-image\nsteps:\n  - id: aa\n    image: alpine\n    agent: {prompt: x}\n"), agentOpts)
	mustHave(t, issues, "/steps/0/image", "image is not valid on agent steps", "profile supplies the image")
	_, issues, _ = Parse([]byte("version: 1\nname: no-prompt\nsteps:\n  - id: aa\n    agent: {profle: x}\n"), agentOpts)
	mustHave(t, issues, "/steps/0/agent/profle", `unknown field "profle"`, `did you mean "profile"?`)
	_, issues, _ = Parse([]byte("version: 1\nname: two-forms\nsteps:\n  - id: aa\n    agent: {prompt: x}\n    uses: slack.post\n"), agentOpts)
	mustHave(t, issues, "/steps/0", "not both agent and uses", "")
	_, issues, _ = Parse([]byte("version: 1\nname: bad-template\nsteps:\n  - id: aa\n    agent: {prompt: \"${{ steps.\"}\n"), agentOpts)
	mustHave(t, issues, "/steps/0/agent/prompt", "invalid expression", "")
}

func TestAgentUntrustedWarning(t *testing.T) {
	y := `version: 1
name: triage
triggers: [{integration: github, event: issues.opened}]
steps:
  - id: triage
    agent:
      profile: coder
      prompt: "Triage: ${{ trigger.title }}"
  - id: quiet
    agent: {prompt: "Summarize the repository"}
  - id: other
    image: alpine
    command: echo "${{ trigger.title }}"
  - id: default
    agent: {prompt: "${{ trigger.body }}"}
`
	spec, issues, err := Parse([]byte(y), agentOpts)
	if err != nil || len(issues) > 0 {
		t.Fatalf("a warning must not block: %v %v", err, issues)
	}
	ws := Warnings(spec, agentOpts)
	if len(ws) != 2 {
		t.Fatalf("%v", ws)
	}
	mustHave(t, ws, "/steps/0/agent/prompt", "profile coder's egress allowlist and credentials", "untrusted")
	mustHave(t, ws, "/steps/3/agent/prompt", "profile reviewer's", "")
	// Without the config, the step's own profile name, or none.
	ws = Warnings(spec, Options{})
	mustHave(t, ws, "/steps/0/agent/prompt", "profile coder's", "")
	mustHave(t, ws, "/steps/3/agent/prompt", "its profile's", "")
	if Warnings(nil, agentOpts) != nil {
		t.Fatal("nil spec")
	}
	spec.Steps[0].Agent.Prompt = "${{ trigger."
	if len(Warnings(spec, Options{})) != 1 {
		t.Fatal("an unparseable prompt is the validator's business")
	}
}
