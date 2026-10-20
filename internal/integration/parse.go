package integration

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"icaro/internal/expr"
)

//go:embed manifest.schema.json
var schemaJSON []byte

// SchemaID is the $id of the manifest schema.
const SchemaID = "https://icaro.dev/schemas/integration-manifest/v1.json"

// SchemaJSON returns the published manifest JSON Schema.
func SchemaJSON() []byte { return schemaJSON }

var compiledSchema = func() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schemaJSON)))
	if err != nil {
		panic(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(SchemaID, doc); err != nil {
		panic(err)
	}
	return c.MustCompile(SchemaID)
}()

var printer = message.NewPrinter(language.English)

// Issue is one manifest problem, addressed by JSON Pointer (same shape as
// workflow issues so every surface shows them alike).
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

// Issues is a list of problems.
type Issues []Issue

func (is Issues) Error() string {
	parts := make([]string, len(is))
	for i, it := range is {
		parts[i] = it.String()
	}
	return strings.Join(parts, "; ")
}

// Parse decodes and validates a manifest. A non-nil error means the input
// is not YAML at all; issues describe everything else.
func Parse(src []byte) (*Manifest, Issues, error) {
	var doc any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse yaml: %w", err)
	}
	doc = normalizeYAML(doc)
	if doc == nil {
		return nil, Issues{{Path: "/", Message: "document is empty"}}, nil
	}
	if _, ok := doc.(map[string]any); !ok {
		return nil, Issues{{Path: "/", Message: "document must be a mapping"}}, nil
	}
	var issues Issues
	if err := compiledSchema.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return nil, nil, err
		}
		issues = append(issues, schemaIssues(ve)...)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	m := &Manifest{}
	if err := json.Unmarshal(raw, m); err != nil {
		if len(issues) == 0 {
			issues = append(issues, Issue{Path: "/", Message: err.Error()})
		}
		return m, issues, nil
	}
	if len(issues) == 0 {
		issues = append(issues, m.Validate()...)
	}
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Path < issues[j].Path })
	return m, issues, nil
}

// Validate applies the semantic rules the schema cannot express.
func (m *Manifest) Validate() Issues {
	v := &validator{m: m}
	if m.Triggers != nil {
		v.triggers(m.Triggers)
	}
	for _, name := range sortedKeys(m.Auth) {
		a := m.Auth[name]
		p := "/auth/" + name
		switch a.Kind {
		case AuthNone:
		default:
			if a.Credential == "" {
				v.add(p+"/credential", fmt.Sprintf("auth kind %q needs a credential", a.Kind))
			}
		}
		switch a.Kind {
		case AuthHeader:
			if a.Header == "" {
				v.add(p+"/header", "auth kind header needs a header name")
			}
		case AuthQuery:
			if a.Param == "" {
				v.add(p+"/param", "auth kind query needs a param name")
			}
		case AuthBasic:
			if a.Username != "" {
				v.template(p+"/username", a.Username, RootsRequest, nil)
			}
		}
	}
	for _, name := range sortedKeys(m.Actions) {
		v.action(name, m.Actions[name])
	}
	return v.issues
}

type validator struct {
	m      *Manifest
	issues Issues
}

func (v *validator) add(path, msg string, hint ...string) {
	is := Issue{Path: path, Message: msg}
	if len(hint) > 0 {
		is.Hint = hint[0]
	}
	v.issues = append(v.issues, is)
}

func (v *validator) triggers(t *Triggers) {
	vf := t.Verify
	switch vf.Kind {
	case VerifyHMACSHA256, VerifyHMACSHA1, VerifySharedSecret:
		if vf.Header == "" {
			v.add("/triggers/verify/header", fmt.Sprintf("verify kind %q needs the header that carries the signature", vf.Kind))
		}
	case VerifyURLToken:
		if vf.Header != "" || vf.Message != "" {
			v.add("/triggers/verify", "url-token verification takes no header or message")
		}
	}
	if vf.Message != "" {
		v.template("/triggers/verify/message", vf.Message, RootsTrigger, nil)
	}
	if t.Timestamp != nil && t.Timestamp.MaxSkew != "" {
		if d, err := time.ParseDuration(t.Timestamp.MaxSkew); err != nil || d <= 0 {
			v.add("/triggers/timestamp/max_skew", "must be a positive duration such as 5m")
		}
	}
	if t.DeliveryID != "" {
		v.template("/triggers/delivery_id", t.DeliveryID, RootsTrigger, nil)
	}
	for i, c := range t.Challenges {
		p := fmt.Sprintf("/triggers/challenges/%d", i)
		v.condition(p+"/match", c.Match, RootsTrigger)
		v.template(p+"/body", c.Body, RootsTrigger, nil)
	}
	for _, name := range sortedKeys(t.Events) {
		ev := t.Events[name]
		p := "/triggers/events/" + escapePtr(name)
		v.condition(p+"/match", ev.Match, RootsTrigger)
		for _, field := range sortedKeys(ev.Payload) {
			v.template(p+"/payload/"+field, ev.Payload[field], RootsTrigger, nil)
		}
	}
}

func (v *validator) action(name string, a Action) {
	p := "/actions/" + name
	inputs := sortedKeys(a.Inputs)
	for _, in := range inputs {
		def := a.Inputs[in].Default
		if def != nil && !typeMatches(a.Inputs[in].Type, def) {
			v.add(p+"/inputs/"+in+"/default", fmt.Sprintf("default does not match type %s", a.Inputs[in].Type))
		}
	}
	check := func(path, s string, roots []string) {
		v.template(path, s, roots, func(path string, r expr.Ref) {
			switch r.Path[0] {
			case "input":
				if len(r.Path) > 1 {
					if _, ok := a.Inputs[r.Path[1]]; !ok {
						is := Issue{Path: path, Message: fmt.Sprintf("references input.%s, which is not a declared input", r.Path[1])}
						if h := expr.Nearest(r.Path[1], inputs); h != "" {
							is.Hint = "did you mean input." + h + "?"
						}
						v.issues = append(v.issues, is)
					}
				}
			case "config":
				if len(r.Path) > 1 {
					if _, ok := v.m.Config[r.Path[1]]; !ok {
						v.add(path, fmt.Sprintf("references config.%s, which is not declared under config", r.Path[1]))
					}
				}
			}
		})
	}
	rq := a.Request
	check(p+"/request/url", rq.URL, RootsRequest)
	v.checkURLHost(p+"/request/url", rq.URL)
	for _, k := range sortedKeys(rq.Headers) {
		check(p+"/request/headers/"+escapePtr(k), rq.Headers[k], RootsRequest)
		if strings.EqualFold(k, "authorization") {
			v.add(p+"/request/headers/"+escapePtr(k), "set credentials through request.auth, not a literal Authorization header")
		}
	}
	for _, k := range sortedKeys(rq.Query) {
		check(p+"/request/query/"+escapePtr(k), rq.Query[k], RootsRequest)
	}
	if rq.Body != nil {
		n := 0
		if rq.Body.JSON != nil {
			n++
			expr.WalkTemplates(rq.Body.JSON, func(ptr, s string) { check(p+"/request/body/json"+ptr, s, RootsRequest) })
		}
		if rq.Body.Form != nil {
			n++
			for _, k := range sortedKeys(rq.Body.Form) {
				check(p+"/request/body/form/"+escapePtr(k), rq.Body.Form[k], RootsRequest)
			}
		}
		if rq.Body.Raw != "" {
			n++
			check(p+"/request/body/raw", rq.Body.Raw, RootsRequest)
		}
		if n != 1 {
			v.add(p+"/request/body", "body needs exactly one of json, form, raw")
		}
	}
	if rq.Auth != "" {
		if _, ok := v.m.Auth[rq.Auth]; !ok {
			is := Issue{Path: p + "/request/auth", Message: fmt.Sprintf("unknown auth %q", rq.Auth)}
			if h := expr.Nearest(rq.Auth, sortedKeys(v.m.Auth)); h != "" {
				is.Hint = fmt.Sprintf("did you mean %q?", h)
			}
			v.issues = append(v.issues, is)
		}
	}
	for _, k := range sortedKeys(a.Outputs) {
		check(p+"/outputs/"+k, a.Outputs[k], RootsOutput)
	}
	if a.Success != "" {
		v.condition(p+"/success", a.Success, RootsOutput)
	}
}

// checkURLHost requires the URL's host to be declared in hosts: either a
// literal host, or ${{ config.base_url }} whose default host is declared.
func (v *validator) checkURLHost(path, raw string) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "${{") {
		end := strings.Index(s, "}}")
		if end < 0 {
			return
		}
		inner := strings.TrimSpace(s[3:end])
		if !strings.HasPrefix(inner, "config.") {
			v.add(path, "the URL must start with https://<host> or ${{ config.<key> }} so its host can be checked against hosts")
			return
		}
		key := strings.TrimPrefix(inner, "config.")
		cf, ok := v.m.Config[key]
		if !ok || cf.Default == "" {
			return // reported elsewhere / user-supplied
		}
		s = cf.Default
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || expr.Contains(u.Host) {
		v.add(path, "the URL must start with a literal scheme and host")
		return
	}
	if u.Scheme != "https" {
		v.add(path, "action URLs must use https")
	}
	if !hostDeclared(v.m.Hosts, u.Hostname()) {
		v.add(path, fmt.Sprintf("host %q is not declared in hosts", u.Hostname()), "add it to the manifest's hosts list")
	}
}

func hostDeclared(hosts []string, host string) bool {
	host = strings.ToLower(host)
	for _, h := range hosts {
		if h == host || (strings.HasPrefix(h, "*.") && strings.HasSuffix(host, h[1:])) {
			return true
		}
	}
	return false
}

func (v *validator) condition(path, src string, roots []string) {
	e, err := expr.ParseCondition(src)
	if err != nil {
		v.add(path, "expression error: "+err.Error())
		return
	}
	if err := expr.CheckRoots(e.Refs(), roots); err != nil {
		v.add(path, err.Error())
	}
}

func (v *validator) template(path, src string, roots []string, each func(string, expr.Ref)) {
	if !expr.Contains(src) {
		return
	}
	t, err := expr.ParseTemplate(src)
	if err != nil {
		v.add(path, "expression error: "+err.Error())
		return
	}
	refs := t.Refs()
	if err := expr.CheckRoots(refs, roots); err != nil {
		v.add(path, err.Error())
		return
	}
	if each != nil {
		for _, r := range refs {
			each(path, r)
		}
	}
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		switch v.(type) {
		case float64, int, int64:
			return true
		}
		return false
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	}
	return false
}

func schemaIssues(ve *jsonschema.ValidationError) Issues {
	var out Issues
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		path := "/" + strings.Join(e.InstanceLocation, "/")
		is := Issue{Path: path, Message: e.ErrorKind.LocalizedString(printer)}
		switch k := e.ErrorKind.(type) {
		case *kind.AdditionalProperties:
			for _, p := range k.Properties {
				out = append(out, Issue{Path: strings.TrimSuffix(path, "/") + "/" + p, Message: fmt.Sprintf("unknown field %q", p)})
			}
			return
		case *kind.Required:
			is.Message = "missing required field(s): " + strings.Join(k.Missing, ", ")
		case *kind.Enum:
			want := make([]string, len(k.Want))
			for i, w := range k.Want {
				want[i] = fmt.Sprint(w)
			}
			is.Message = "value is not one of the allowed values"
			is.Hint = "allowed: " + strings.Join(want, ", ")
			if gs, ok := k.Got.(string); ok {
				if h := expr.Nearest(gs, want); h != "" {
					is.Hint = fmt.Sprintf("did you mean %q? allowed: %s", h, strings.Join(want, ", "))
				}
			}
		}
		out = append(out, is)
	}
	walk(ve)
	return out
}

func normalizeYAML(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, val := range v {
			v[k] = normalizeYAML(val)
		}
		return v
	case map[any]any:
		m := make(map[string]any, len(v))
		for k, val := range v {
			m[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return m
	case []any:
		for i, val := range v {
			v[i] = normalizeYAML(val)
		}
		return v
	case int:
		return float64(v)
	default:
		return v
	}
}

func escapePtr(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
