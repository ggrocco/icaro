package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"icaro/internal/expr"
)

var printer = message.NewPrinter(language.English)

// Issue is one validation problem, addressed by JSON Pointer. The CLI,
// API, UI and the MCP validate_workflow tool all show this list.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (i Issue) String() string {
	if i.Hint != "" {
		return fmt.Sprintf("%s: %s (hint: %s)", i.Path, i.Message, i.Hint)
	}
	return fmt.Sprintf("%s: %s", i.Path, i.Message)
}

// Issues is a list of validation problems.
type Issues []Issue

func (is Issues) Error() string {
	parts := make([]string, len(is))
	for i, it := range is {
		parts[i] = it.String()
	}
	return strings.Join(parts, "; ")
}

// Options parameterize validation with knowledge of the environment.
type Options struct {
	// MaxTimeout caps step timeouts (config limits.timeout). Zero disables.
	MaxTimeout time.Duration
	// Catalog resolves uses actions and integration events. Nil skips
	// those checks.
	Catalog Catalog
	// SecretExists reports whether a secret exists. Nil skips the check.
	SecretExists func(name string) bool
	// SecretBound reports whether a secret has a host binding (an
	// integration credential). Nil skips the binding check; credentials
	// named after an integration in Catalog are refused regardless.
	SecretBound func(name string) bool
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseCron parses a cron trigger expression (5 fields or @descriptor).
func ParseCron(s string) (cron.Schedule, error) { return cronParser.Parse(s) }

var compiledSchema = func() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(SchemaJSON())))
	if err != nil {
		panic(err)
	}
	if err := c.AddResource(SchemaID, doc); err != nil {
		panic(err)
	}
	return c.MustCompile(SchemaID)
}()

// Parse decodes YAML (or JSON), validates it against the schema and the
// semantic rules, and returns the spec even when issues are found. A
// non-nil error means the input could not be decoded at all.
func Parse(src []byte, opts Options) (*Spec, Issues, error) {
	var doc any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse yaml: %w", err)
	}
	doc = normalize(doc)
	if doc == nil {
		return nil, Issues{{Path: "/", Message: "document is empty"}}, nil
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, Issues{{Path: "/", Message: "document must be a mapping"}}, nil
	}
	var issues Issues
	if err := compiledSchema.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return nil, nil, err
		}
		issues = append(issues, schemaIssues(ve, m)...)
	}
	spec := &Spec{}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(raw, spec); err != nil {
		if len(issues) == 0 {
			issues = append(issues, Issue{Path: "/", Message: err.Error()})
		}
		return spec, sortIssues(issues), nil
	}
	if len(issues) == 0 {
		issues = append(issues, spec.Validate(opts)...)
	}
	return spec, sortIssues(issues), nil
}

func sortIssues(is Issues) Issues {
	sort.SliceStable(is, func(i, j int) bool { return is[i].Path < is[j].Path })
	return dedupeIssues(is)
}

func dedupeIssues(is Issues) Issues {
	seen := map[Issue]bool{}
	out := is[:0]
	for _, i := range is {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	return out
}

var outputKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$`)

// validator accumulates issues for one spec.
type validator struct {
	s      *Spec
	opts   Options
	issues Issues
	index  map[string]int
	plan   *Plan
}

// addExpr records an expression error, moving a nearest-match suggestion
// from the message into the hint.
func (v *validator) addExpr(path, prefix string, err error) {
	msg, hint := err.Error(), ""
	var pe *expr.Error
	if errors.As(err, &pe) {
		msg = pe.Msg
		if i := strings.Index(msg, "; did you mean"); i >= 0 {
			msg, hint = msg[:i], strings.TrimPrefix(msg[i:], "; ")
		}
		if pe.Pos >= 0 && pe.Src != "" {
			msg = fmt.Sprintf("%s (at offset %d in %q)", msg, pe.Pos, pe.Src)
		}
	}
	if hint != "" {
		v.add(path, prefix+msg, hint)
	} else {
		v.add(path, prefix+msg)
	}
}

func (v *validator) add(path, msg string, hint ...string) {
	is := Issue{Path: path, Message: msg}
	if len(hint) > 0 {
		is.Hint = hint[0]
	}
	v.issues = append(v.issues, is)
}

// Validate applies the semantic rules the schema cannot express.
func (s *Spec) Validate(opts Options) Issues {
	v := &validator{s: s, opts: opts, index: map[string]int{}}
	if s.Version != 1 {
		v.add("/version", fmt.Sprintf("unsupported version %d", s.Version), "use version: 1")
	}
	v.checkSteps()
	v.checkTriggers()
	v.checkWorkflowEnv()
	v.checkExpressions()
	return v.issues
}

func (v *validator) stepPath(i int) string { return fmt.Sprintf("/steps/%d", i) }

func (v *validator) checkSteps() {
	s := v.s
	for i, st := range s.Steps {
		base := v.stepPath(i)
		if prev, dup := v.index[st.ID]; dup {
			v.add(base+"/id", fmt.Sprintf("duplicate step id %q (also used by step %d)", st.ID, prev))
			continue
		}
		v.index[st.ID] = i
	}
	ids := s.StepIDs()
	needsOK := true
	for i, st := range s.Steps {
		base := v.stepPath(i)
		for j, n := range st.Needs {
			switch {
			case n == st.ID:
				v.add(fmt.Sprintf("%s/needs/%d", base, j), "a step cannot need itself")
				needsOK = false
			case !contains(ids, n):
				msg := fmt.Sprintf("unknown step %q", n)
				if h := expr.Nearest(n, ids); h != "" {
					v.add(fmt.Sprintf("%s/needs/%d", base, j), msg, fmt.Sprintf("did you mean %q?", h))
				} else {
					v.add(fmt.Sprintf("%s/needs/%d", base, j), msg, "known steps: "+strings.Join(ids, ", "))
				}
				needsOK = false
			}
		}
		v.checkStepShape(i, &s.Steps[i])
	}
	if !needsOK || len(v.index) != len(s.Steps) {
		return
	}
	p, err := NewPlan(s)
	var ce *CycleError
	if errors.As(err, &ce) {
		first := v.index[ce.Path[0]]
		v.add(v.stepPath(first)+"/needs", ce.Error(), "remove one of the needs edges in the cycle")
		return
	}
	if err != nil {
		v.add("/steps", err.Error())
		return
	}
	v.plan = p
}

func (v *validator) checkStepShape(i int, st *StepSpec) {
	base := v.stepPath(i)
	switch st.Kind() {
	case "":
		v.add(base, "a step must have exactly one of: image, uses")
		return
	case KindUses:
		if st.Command != "" {
			v.add(base+"/command", "command is only valid on image steps", "uses steps take their parameters in input")
		}
		if len(st.Env) > 0 {
			v.add(base+"/env", "env is only valid on image steps", "pass values to the action through input")
		}
		if len(st.Outputs) > 0 {
			v.add(base+"/outputs", "outputs are declared by the action's manifest; remove this field")
		}
		if st.Repository != nil {
			v.add(base+"/repository", "repository is only valid on image steps; uses steps run in the daemon")
		}
		v.checkAction(i, st)
	case KindImage:
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
	if st.Timeout != "" {
		d, err := time.ParseDuration(st.Timeout)
		switch {
		case err != nil || d <= 0:
			v.add(base+"/timeout", "must be a positive duration such as 90s, 10m or 1h")
		case v.opts.MaxTimeout > 0 && d > v.opts.MaxTimeout:
			v.add(base+"/timeout", fmt.Sprintf("exceeds the configured maximum of %s", v.opts.MaxTimeout), "lower it or raise limits.timeout in the config")
		}
	}
	if st.Retry < 0 || st.Retry > 5 {
		v.add(base+"/retry", "retry must be between 0 and 5")
	}
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (v *validator) checkAction(i int, st *StepSpec) {
	if v.opts.Catalog == nil {
		return
	}
	base := v.stepPath(i)
	info, ok := v.opts.Catalog.Action(st.Uses)
	if !ok {
		msg := fmt.Sprintf("unknown action %q", st.Uses)
		if h := expr.Nearest(st.Uses, v.opts.Catalog.ActionRefs()); h != "" {
			v.add(base+"/uses", msg, fmt.Sprintf("did you mean %q?", h))
		} else {
			v.add(base+"/uses", msg, "install the integration pack that provides it (icaro integration list)")
		}
		return
	}
	known := sortedKeys(info.Inputs)
	for _, k := range sortedKeys(st.Input) {
		decl, ok := info.Inputs[k]
		if !ok {
			msg := fmt.Sprintf("unknown input %q for %s", k, st.Uses)
			if h := expr.Nearest(k, known); h != "" {
				v.add(base+"/input/"+escape(k), msg, fmt.Sprintf("did you mean %q?", h))
			} else {
				v.add(base+"/input/"+escape(k), msg, "inputs: "+strings.Join(known, ", "))
			}
			continue
		}
		if err := checkInputType(decl.Type, st.Input[k]); err != nil {
			v.add(base+"/input/"+escape(k), err.Error())
		}
	}
	for _, k := range known {
		decl := info.Inputs[k]
		if _, set := st.Input[k]; decl.Required && !set && decl.Default == nil {
			v.add(base+"/input", fmt.Sprintf("missing required input %q for %s", k, st.Uses))
		}
	}
}

// checkInputType checks a literal input against its declared type;
// templated values are checked at run time.
func checkInputType(typ string, val any) error {
	if s, ok := val.(string); ok && expr.Contains(s) {
		return nil
	}
	ok := true
	switch typ {
	case "", "any":
	case "string":
		_, ok = val.(string)
	case "number", "integer":
		_, ok = val.(float64)
		if !ok {
			_, ok = val.(int)
		}
	case "boolean":
		_, ok = val.(bool)
	case "object":
		_, ok = val.(map[string]any)
	case "array":
		_, ok = val.([]any)
	}
	if !ok {
		return fmt.Errorf("input must be of type %s", typ)
	}
	return nil
}

func (v *validator) checkTriggers() {
	for i, t := range v.s.Triggers {
		base := fmt.Sprintf("/triggers/%d", i)
		switch {
		case t.Type == TriggerCron:
			if strings.TrimSpace(t.Cron) == "" {
				v.add(base+"/cron", "a cron trigger needs a cron expression", `e.g. cron: "0 9 * * 1-5"`)
			} else if _, err := ParseCron(t.Cron); err != nil {
				v.add(base+"/cron", fmt.Sprintf("invalid cron expression: %v", err), "5 fields: minute hour day-of-month month day-of-week")
			}
		case t.Type == TriggerManual:
			if t.Cron != "" {
				v.add(base+"/cron", "cron is only valid on cron triggers")
			}
		case t.Integration != "":
			v.checkIntegrationTrigger(base, t)
		}
	}
}

func (v *validator) checkIntegrationTrigger(base string, t TriggerSpec) {
	var info *EventInfo
	if v.opts.Catalog != nil {
		if ev, ok := v.opts.Catalog.Event(t.Integration, t.Event); ok {
			info = &ev
		} else if !contains(v.opts.Catalog.Integrations(), t.Integration) {
			msg := fmt.Sprintf("unknown integration %q", t.Integration)
			if h := expr.Nearest(t.Integration, v.opts.Catalog.Integrations()); h != "" {
				v.add(base+"/integration", msg, fmt.Sprintf("did you mean %q?", h))
			} else {
				v.add(base+"/integration", msg, "installed: "+strings.Join(v.opts.Catalog.Integrations(), ", "))
			}
		} else {
			events := v.opts.Catalog.EventNames(t.Integration)
			msg := fmt.Sprintf("integration %s has no event %q", t.Integration, t.Event)
			if h := expr.Nearest(t.Event, events); h != "" {
				v.add(base+"/event", msg, fmt.Sprintf("did you mean %q?", h))
			} else {
				v.add(base+"/event", msg, "events: "+strings.Join(events, ", "))
			}
		}
	}
	if strings.TrimSpace(t.Filter) == "" {
		return
	}
	e, err := expr.ParseCondition(t.Filter)
	if err != nil {
		v.addExpr(base+"/filter", "invalid expression: ", err)
		return
	}
	refs := e.Refs()
	if err := expr.CheckRoots(refs, RootsFilter); err != nil {
		v.addExpr(base+"/filter", "", err)
		return
	}
	if info != nil {
		for _, r := range refs {
			if len(r.Path) >= 2 && !r.Guarded && !contains(info.Fields, r.Path[1]) {
				v.add(base+"/filter", fmt.Sprintf("%s/%s does not provide trigger.%s", t.Integration, t.Event, r.Path[1]), fieldHint(r.Path[1], info.Fields))
			}
		}
	}
}

func fieldHint(name string, fields []string) string {
	if h := expr.Nearest(name, fields); h != "" {
		return fmt.Sprintf("did you mean trigger.%s?", h)
	}
	return "fields: " + strings.Join(fields, ", ")
}

// workflowEnvRoots are the names workflow-level env values may reference.
var workflowEnvRoots = []string{"secrets", "trigger", "run", "workflow"}

func (v *validator) checkWorkflowEnv() {
	for _, k := range sortedKeys(v.s.Env) {
		path := "/env/" + escape(k)
		if !envNameRe.MatchString(k) {
			v.add(path, fmt.Sprintf("invalid environment variable name %q", k))
		}
		v.checkTemplate(path, v.s.Env[k], workflowEnvRoots, -1)
	}
}

// checkExpressions validates every interpolated field of every step.
func (v *validator) checkExpressions() {
	for i, st := range v.s.Steps {
		base := v.stepPath(i)
		if st.Command != "" {
			v.checkTemplate(base+"/command", st.Command, RootsStep, i)
		}
		for _, k := range sortedKeys(st.Env) {
			v.checkTemplate(base+"/env/"+escape(k), st.Env[k], RootsStep, i)
		}
		if st.Input != nil {
			expr.WalkTemplates(map[string]any(st.Input), func(ptr, s string) {
				v.checkTemplate(base+"/input"+ptr, s, RootsStep, i)
			})
		}
		if strings.TrimSpace(st.When) != "" {
			e, err := expr.ParseCondition(st.When)
			if err != nil {
				v.addExpr(base+"/when", "invalid expression: ", err)
			} else {
				v.checkWhenRefs(base+"/when", e.Refs(), i)
			}
		}
	}
}

// checkWhenRefs refuses secrets in a condition: whether a step ran is
// visible in the run, so a `when` on a secret's value would leak it one
// bit per run. Secrets are not even exposed to `when` at run time.
func (v *validator) checkWhenRefs(path string, refs []expr.Ref, step int) {
	rest := refs[:0:0]
	for _, r := range refs {
		if len(r.Path) > 0 && r.Path[0] == "secrets" {
			v.add(path, "when cannot read secrets", "a condition on a secret leaks it through the step's status; branch on env, trigger or step outputs instead")
			continue
		}
		rest = append(rest, r)
	}
	v.checkRefs(path, rest, RootsStep, step)
}

func (v *validator) checkTemplate(path, s string, roots []string, step int) {
	if !expr.Contains(s) {
		return
	}
	t, err := expr.ParseTemplate(s)
	if err != nil {
		v.addExpr(path, "invalid expression: ", err)
		return
	}
	v.checkRefs(path, t.Refs(), roots, step)
}

// checkRefs validates references: roots, needs ancestry, declared
// outputs, workflow env keys, secrets and trigger fields.
func (v *validator) checkRefs(path string, refs []expr.Ref, roots []string, step int) {
	for _, r := range refs {
		if err := expr.CheckRoots([]expr.Ref{r}, roots); err != nil {
			v.addExpr(path, "", err)
			continue
		}
		if len(r.Path) < 2 {
			continue
		}
		switch r.Path[0] {
		case "steps":
			v.checkStepRef(path, r, step)
		case "env":
			if _, ok := v.s.Env[r.Path[1]]; !ok && !r.Guarded {
				msg := fmt.Sprintf("env.%s is not a workflow env variable", r.Path[1])
				if h := expr.Nearest(r.Path[1], sortedKeys(v.s.Env)); h != "" {
					v.add(path, msg, fmt.Sprintf("did you mean env.%s?", h))
				} else {
					v.add(path, msg, "declare it under the top-level env")
				}
			}
		case "secrets":
			if v.integrationCredential(r.Path[1]) {
				v.add(path, fmt.Sprintf("secret %q is an integration credential: it is only sent to its bound hosts by uses: actions and never enters a template or a container", r.Path[1]), IntegrationCredentialHint(r.Path[1]))
				continue
			}
			if v.opts.SecretExists != nil && !r.Guarded && !v.opts.SecretExists(r.Path[1]) {
				v.add(path, fmt.Sprintf("secret %q does not exist", r.Path[1]), "create it with: icaro secret set "+r.Path[1])
			}
		case "trigger":
			v.checkTriggerRef(path, r)
		}
	}
}

func (v *validator) integrationCredential(name string) bool {
	bound := v.opts.SecretBound != nil && v.opts.SecretBound(name)
	var integrations []string
	if v.opts.Catalog != nil {
		integrations = v.opts.Catalog.Integrations()
	}
	return IntegrationCredential(name, bound, integrations)
}

func (v *validator) checkStepRef(path string, r expr.Ref, step int) {
	id := r.Path[1]
	ids := v.s.StepIDs()
	target, ok := v.index[id]
	if !ok {
		msg := fmt.Sprintf("steps.%s: no such step", id)
		if h := expr.Nearest(id, ids); h != "" {
			v.add(path, msg, fmt.Sprintf("did you mean steps.%s?", h))
		} else {
			v.add(path, msg)
		}
		return
	}
	if step >= 0 && v.plan != nil && !v.plan.Ancestors[v.s.Steps[step].ID][id] {
		v.add(path, fmt.Sprintf("steps.%s is not an ancestor of step %s", id, v.s.Steps[step].ID), fmt.Sprintf("add %q to needs", id))
		return
	}
	if len(r.Path) < 3 {
		return
	}
	switch r.Path[2] {
	case "outputs":
	case "status":
		return
	default:
		msg := fmt.Sprintf("steps.%s.%s: a step exposes outputs and status", id, r.Path[2])
		if h := expr.Nearest(r.Path[2], []string{"outputs", "status"}); h != "" {
			v.add(path, msg, fmt.Sprintf("did you mean steps.%s.%s?", id, h))
		} else {
			v.add(path, msg)
		}
		return
	}
	if len(r.Path) < 4 || r.Guarded {
		return
	}
	key := r.Path[3]
	ts := &v.s.Steps[target]
	var declared []string
	switch ts.Kind() {
	case KindImage:
		declared = sortedKeys(ts.Outputs)
	case KindUses:
		if v.opts.Catalog == nil {
			return
		}
		info, ok := v.opts.Catalog.Action(ts.Uses)
		if !ok {
			return
		}
		declared = info.Outputs
	default:
		return
	}
	if contains(declared, key) {
		return
	}
	msg := fmt.Sprintf("step %s declares no output %q", id, key)
	if h := expr.Nearest(key, declared); h != "" {
		v.add(path, msg, fmt.Sprintf("did you mean steps.%s.outputs.%s?", id, h))
	} else if ts.Kind() == KindImage {
		v.add(path, msg, fmt.Sprintf("declare it under steps[%d].outputs, e.g. %s: json:%s", target, key, key))
	} else {
		v.add(path, msg, "outputs: "+strings.Join(declared, ", "))
	}
}

// checkTriggerRef requires trigger.<field> to be provided by every
// integration event the workflow subscribes to, when all are known.
func (v *validator) checkTriggerRef(path string, r expr.Ref) {
	if r.Guarded || v.opts.Catalog == nil {
		return
	}
	triggers := v.s.IntegrationTriggers()
	events := make([]EventInfo, len(triggers))
	for i, t := range triggers {
		ev, ok := v.opts.Catalog.Event(t.Integration, t.Event)
		if !ok {
			return
		}
		events[i] = ev
	}
	for i, t := range triggers {
		if !contains(events[i].Fields, r.Path[1]) {
			v.add(path, fmt.Sprintf("%s/%s does not provide trigger.%s", t.Integration, t.Event, r.Path[1]), fieldHint(r.Path[1], events[i].Fields)+"; guard optional fields with has(trigger."+r.Path[1]+")")
			return
		}
	}
}

// TriggerFields returns the top-level trigger.* keys a workflow
// references: required ones (unguarded) and optional ones (inside has()).
// Manual runs prompt for the required ones.
func TriggerFields(s *Spec) (required, optional []string) {
	req, opt := map[string]bool{}, map[string]bool{}
	visit := func(refs []expr.Ref) {
		for _, r := range refs {
			if len(r.Path) >= 2 && r.Path[0] == "trigger" {
				if r.Guarded {
					opt[r.Path[1]] = true
				} else {
					req[r.Path[1]] = true
				}
			}
		}
	}
	tmpl := func(s string) {
		if t, err := expr.ParseTemplate(s); err == nil {
			visit(t.Refs())
		}
	}
	for _, val := range s.Env {
		tmpl(val)
	}
	for _, st := range s.Steps {
		tmpl(st.Command)
		for _, val := range st.Env {
			tmpl(val)
		}
		if st.Input != nil {
			expr.WalkTemplates(map[string]any(st.Input), func(_, s string) { tmpl(s) })
		}
		if st.When != "" {
			if e, err := expr.ParseCondition(st.When); err == nil {
				visit(e.Refs())
			}
		}
	}
	for k := range req {
		delete(opt, k)
	}
	return sortedKeys(req), sortedKeys(opt)
}

// SecretRefs returns the secret names a workflow references.
func SecretRefs(s *Spec) []string {
	names := map[string]bool{}
	tmpl := func(str string) {
		if t, err := expr.ParseTemplate(str); err == nil {
			for _, r := range t.Refs() {
				if len(r.Path) >= 2 && r.Path[0] == "secrets" {
					names[r.Path[1]] = true
				}
			}
		}
	}
	for _, val := range s.Env {
		tmpl(val)
	}
	for _, st := range s.Steps {
		tmpl(st.Command)
		for _, val := range st.Env {
			tmpl(val)
		}
		if st.Input != nil {
			expr.WalkTemplates(map[string]any(st.Input), func(_, s string) { tmpl(s) })
		}
	}
	return sortedKeys(names)
}

// schemaIssues flattens a validation error tree into leaf issues.
func schemaIssues(ve *jsonschema.ValidationError, doc map[string]any) Issues {
	var out Issues
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		path := "/" + strings.Join(e.InstanceLocation, "/")
		if len(e.InstanceLocation) == 0 {
			path = "/"
		}
		if _, ok := e.ErrorKind.(*kind.OneOf); ok {
			out = append(out, oneOfIssues(e, path, doc)...)
			return
		}
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		is := Issue{Path: path, Message: e.ErrorKind.LocalizedString(printer)}
		switch k := e.ErrorKind.(type) {
		case *kind.AdditionalProperties:
			known := knownKeysAt(e.InstanceLocation, doc)
			base := path
			if base == "/" {
				base = ""
			}
			for _, p := range k.Properties {
				is := Issue{Path: base + "/" + escape(p), Message: fmt.Sprintf("unknown field %q", p)}
				if hint := expr.Nearest(p, known); hint != "" {
					is.Hint = fmt.Sprintf("did you mean %q?", hint)
				} else if len(known) > 0 {
					is.Hint = "allowed: " + strings.Join(known, ", ")
				}
				out = append(out, is)
			}
			return
		case *kind.Required:
			is.Message = fmt.Sprintf("missing required field(s): %s", strings.Join(k.Missing, ", "))
		case *kind.Enum:
			is.Message = "value is not one of the allowed values"
			want := make([]string, len(k.Want))
			for i, w := range k.Want {
				want[i] = fmt.Sprint(w)
			}
			is.Hint = "allowed: " + strings.Join(want, ", ")
			if gs, ok := k.Got.(string); ok {
				if h := expr.Nearest(gs, want); h != "" {
					is.Hint = fmt.Sprintf("did you mean %q? allowed: %s", h, strings.Join(want, ", "))
				}
			}
		case *kind.Const:
			is.Message = fmt.Sprintf("must be %v", k.Want)
		case *kind.Pattern:
			is.Message = fmt.Sprintf("%q does not match %s", k.Got, k.Want)
		}
		out = append(out, is)
	}
	walk(ve)
	return out
}

// oneOfIssues turns a oneOf failure into a readable issue: steps need
// exactly one of image/uses; triggers are reported against the variant
// their keys select.
func oneOfIssues(e *jsonschema.ValidationError, path string, doc map[string]any) Issues {
	loc := e.InstanceLocation
	obj, _ := valueAt(doc, loc).(map[string]any)
	switch {
	case len(loc) == 2 && loc[0] == "steps":
		_, hasImage := obj["image"]
		_, hasUses := obj["uses"]
		if hasImage && hasUses {
			return Issues{{Path: path, Message: "a step has exactly one of image or uses, not both"}}
		}
		return Issues{{Path: path, Message: "a step needs exactly one of: image, uses", Hint: "image: <container image> runs a container; uses: <integration>.<action> calls an integration"}}
	case len(loc) == 2 && loc[0] == "triggers":
		branch := 1 // integration
		if _, ok := obj["type"]; ok {
			branch = 0
		} else if _, ok := obj["integration"]; !ok {
			if _, ok := obj["event"]; !ok {
				return Issues{{Path: path, Message: "a trigger needs type (manual|cron) or integration + event"}}
			}
		}
		var out Issues
		if branch < len(e.Causes) {
			out = append(out, schemaIssues(e.Causes[branch], doc)...)
		}
		if len(out) == 0 {
			out = append(out, Issue{Path: path, Message: "invalid trigger"})
		}
		return out
	}
	var out Issues
	for _, c := range e.Causes {
		out = append(out, schemaIssues(c, doc)...)
	}
	return out
}

func valueAt(doc any, loc []string) any {
	cur := doc
	for _, seg := range loc {
		switch v := cur.(type) {
		case map[string]any:
			cur = v[seg]
		case []any:
			var n int
			if _, err := fmt.Sscan(seg, &n); err != nil || n < 0 || n >= len(v) {
				return nil
			}
			cur = v[n]
		default:
			return nil
		}
	}
	return cur
}

// knownKeysAt returns the JSON field names valid at an instance location,
// by walking the spec types. A trigger's keys follow its variant.
func knownKeysAt(loc []string, doc map[string]any) []string {
	t := reflect.TypeOf(Spec{})
	for _, seg := range loc {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Slice, reflect.Map:
			t = t.Elem()
		case reflect.Struct:
			f, ok := fieldByJSONName(t, seg)
			if !ok {
				return nil
			}
			t = f.Type
		default:
			return nil
		}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	if t == typeTrigger {
		obj, _ := valueAt(doc, loc).(map[string]any)
		if _, ok := obj["type"]; ok {
			return triggerVariants[0].fields
		}
		return triggerVariants[1].fields
	}
	var names []string
	for i := 0; i < t.NumField(); i++ {
		if n := jsonName(t.Field(i)); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func fieldByJSONName(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if jsonName(f) == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// escape encodes a JSON Pointer token.
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// normalize converts yaml.v3 decoded values into JSON-compatible ones.
func normalize(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, val := range v {
			v[k] = normalize(val)
		}
		return v
	case map[any]any:
		m := make(map[string]any, len(v))
		for k, val := range v {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i, val := range v {
			v[i] = normalize(val)
		}
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	default:
		return v
	}
}
