package workflow

import (
	"fmt"
	"sort"
	"strings"

	"icaro/internal/creds"
	"icaro/internal/expr"
)

// AgentProfile is a profile the operator lists in workflows.agent_profiles
// (ADR 0037): its name and its agent (the harness).
type AgentProfile struct {
	Name  string `json:"name"`
	Agent string `json:"agent"`
}

// AgentOptions is what validation knows about agent steps. Profiles are in
// config order; the first is the default. An empty list means agent steps
// are off.
type AgentOptions struct {
	Profiles []AgentProfile
}

// AgentSettingHint names the setting an operator changes to allow agent
// steps.
const AgentSettingHint = "the operator lists the profiles agent steps may use in workflows.agent_profiles in the config (icaro config show)"

// AgentStepProfile resolves an agent step's profile: its own, or the first
// listed one. ok is false when the profile is not listed.
func (o *AgentOptions) AgentStepProfile(a *AgentSpec) (AgentProfile, bool) {
	if o == nil || len(o.Profiles) == 0 {
		return AgentProfile{}, false
	}
	if a.Profile == "" {
		return o.Profiles[0], true
	}
	for _, p := range o.Profiles {
		if p.Name == a.Profile {
			return p, true
		}
	}
	return AgentProfile{}, false
}

func (o *AgentOptions) names() []string {
	out := make([]string, len(o.Profiles))
	for i, p := range o.Profiles {
		out[i] = p.Name
	}
	return out
}

// checkAgent validates an agent step's form, profile and params.
func (v *validator) checkAgent(i int, st *StepSpec) {
	base := v.stepPath(i)
	a := st.Agent
	if st.Command != "" {
		v.add(base+"/command", "command is not valid on agent steps", "the harness supplies the command; describe the task in agent.prompt")
	}
	if strings.TrimSpace(a.Prompt) == "" {
		v.add(base+"/agent/prompt", "an agent step needs a prompt")
	}
	v.checkOutputsAndEnv(base, st)
	if _, ok := st.Outputs[ResultOutput]; ok {
		v.add(base+"/outputs/"+ResultOutput, fmt.Sprintf("%q is the built-in output of agent steps (the agent's final answer); pick another name", ResultOutput))
	}
	o := v.opts.Agents
	if o == nil {
		return
	}
	path := base + "/agent"
	if a.Profile != "" {
		path += "/profile"
	}
	if len(o.Profiles) == 0 {
		v.add(path, "agent steps are off: no profile is listed in workflows.agent_profiles", AgentSettingHint)
		return
	}
	prof, ok := o.AgentStepProfile(a)
	if !ok {
		msg := fmt.Sprintf("profile %q is not listed in workflows.agent_profiles", a.Profile)
		if h := expr.Nearest(a.Profile, o.names()); h != "" {
			v.add(path, msg, fmt.Sprintf("did you mean %q? (%s)", h, AgentSettingHint))
		} else {
			v.add(path, msg, "listed: "+strings.Join(o.names(), ", ")+" ("+AgentSettingHint+")")
		}
		return
	}
	agent, err := creds.LookupAgent(prof.Agent)
	if err != nil {
		v.add(path, err.Error())
		return
	}
	spec, err := agent.Step()
	if err != nil {
		v.add(path, fmt.Sprintf("profile %q: %v", prof.Name, err))
		return
	}
	keys := make([]string, 0, len(a.Params))
	for k := range a.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pp := base + "/agent/params/" + escape(k)
		p, ok := spec.Param(k)
		if !ok {
			msg := fmt.Sprintf("%s has no param %q", agent.Name, k)
			if h := expr.Nearest(k, spec.ParamNames()); h != "" {
				v.add(pp, msg, fmt.Sprintf("did you mean %q?", h))
			} else {
				v.add(pp, msg, "params: "+strings.Join(spec.ParamNames(), ", "))
			}
			continue
		}
		if err := p.CheckParam(a.Params[k]); err != nil {
			v.add(pp, fmt.Sprintf("param %s %v", k, err), p.Description)
		}
	}
}

// checkOutputsAndEnv validates declared outputs and env names (image and
// agent steps).
func (v *validator) checkOutputsAndEnv(base string, st *StepSpec) {
	for _, k := range sortedKeys(st.Outputs) {
		if !outputKeyRe.MatchString(k) {
			v.add(base+"/outputs/"+escape(k), fmt.Sprintf("invalid output name %q", k), "use letters, digits, '_' and '-'")
		}
		if _, err := ParseCapture(st.Outputs[k]); err != nil {
			v.add(base+"/outputs/"+escape(k), err.Error(), "one of "+CaptureForms)
		}
	}
	for _, k := range sortedKeys(st.Env) {
		if !envNameRe.MatchString(k) {
			v.add(base+"/env/"+escape(k), fmt.Sprintf("invalid environment variable name %q", k))
		}
	}
	if st.Repository != nil && strings.TrimSpace(st.Repository.Ref) == "" {
		v.add(base+"/repository/ref", "repository ref is empty")
	}
}

// checkPrompt validates an agent prompt's references: the prompt roots,
// with secrets refused by name.
func (v *validator) checkPrompt(path, s string, step int) {
	if !expr.Contains(s) {
		return
	}
	t, err := expr.ParseTemplate(s)
	if err != nil {
		v.addExpr(path, "invalid expression: ", err)
		return
	}
	rest := t.Refs()[:0:0]
	for _, r := range t.Refs() {
		if len(r.Path) > 0 && r.Path[0] == "secrets" {
			v.add(path, "the prompt cannot read secrets", "the prompt is stored, shown and sent to the provider; pass a secret the agent needs through the step's env")
			continue
		}
		rest = append(rest, r)
	}
	v.checkRefs(path, rest, RootsPrompt, step)
}

// Warnings reports what is valid but worth a second look: today, agent
// steps whose prompt carries text from an integration trigger, which an
// outsider may have written (ADR 0037). They never block a save.
func Warnings(s *Spec, opts Options) Issues {
	var out Issues
	if s == nil || len(s.IntegrationTriggers()) == 0 {
		return out
	}
	for i, st := range s.Steps {
		if st.Agent == nil || !expr.Contains(st.Agent.Prompt) {
			continue
		}
		t, err := expr.ParseTemplate(st.Agent.Prompt)
		if err != nil {
			continue
		}
		reads := false
		for _, r := range t.Refs() {
			reads = reads || (len(r.Path) > 0 && r.Path[0] == "trigger")
		}
		if !reads {
			continue
		}
		name := st.Agent.Profile
		if p, ok := opts.Agents.AgentStepProfile(st.Agent); ok {
			name = p.Name
		}
		who := "its profile"
		if name != "" {
			who = "profile " + name
		}
		out = append(out, Issue{
			Path:    fmt.Sprintf("/steps/%d/agent/prompt", i),
			Message: fmt.Sprintf("the prompt includes trigger data from an integration event, which an outsider may have written; the agent runs with %s's egress allowlist and credentials", who),
			Hint:    "treat the trigger text as untrusted: keep the profile's allowlist narrow and pass only what the step needs",
		})
	}
	return out
}
