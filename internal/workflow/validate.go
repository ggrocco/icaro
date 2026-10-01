package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var printer = message.NewPrinter(language.English)

// Issue is one validation problem, addressed by JSON pointer.
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

// Options parameterize validation with environment knowledge.
type Options struct {
	// ConnectionExists reports whether a named connection exists. Nil skips the check.
	ConnectionExists func(name string) bool
	// MaxTimeout caps step timeouts. Zero disables the check.
	MaxTimeout time.Duration
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

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

// Parse decodes YAML (or JSON) into a Workflow, validating it against the
// schema and the semantic rules. It returns the decoded workflow even when
// issues are found so callers can show partial information; a non-nil error
// means the input could not be decoded at all.
func Parse(src []byte, opts Options) (*Workflow, Issues, error) {
	var doc any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse yaml: %w", err)
	}
	doc = normalize(doc)
	if doc == nil {
		return nil, Issues{{Path: "/", Message: "document is empty"}}, nil
	}
	if _, ok := doc.(map[string]any); !ok {
		return nil, Issues{{Path: "/", Message: "document must be a mapping"}}, nil
	}

	var issues Issues
	if err := compiledSchema.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			issues = append(issues, schemaIssues(ve)...)
		} else {
			return nil, nil, err
		}
	}

	wf := &Workflow{}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(raw, wf); err != nil {
		// Shape errors are already reported by the schema pass; decoding
		// failures here mean the schema missed something.
		if len(issues) == 0 {
			issues = append(issues, Issue{Path: "/", Message: err.Error()})
		}
		return wf, issues, nil
	}
	if len(issues) == 0 {
		issues = append(issues, wf.Validate(opts)...)
	}
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Path < issues[j].Path })
	return wf, issues, nil
}

// Validate applies the semantic rules that the schema cannot express.
func (w *Workflow) Validate(opts Options) Issues {
	var issues Issues
	add := func(path, msg string, hint ...string) {
		is := Issue{Path: path, Message: msg}
		if len(hint) > 0 {
			is.Hint = hint[0]
		}
		issues = append(issues, is)
	}

	if w.On != nil {
		for i, s := range w.On.Schedule {
			if _, err := cronParser.Parse(s.Cron); err != nil {
				add(fmt.Sprintf("/on/schedule/%d/cron", i), fmt.Sprintf("invalid cron expression: %v", err))
			}
		}
	}

	names := map[string]int{}
	for i, st := range w.Steps {
		base := fmt.Sprintf("/steps/%d", i)
		if prev, dup := names[st.Name]; dup {
			add(base+"/name", fmt.Sprintf("duplicate step name %q (also used by step %d)", st.Name, prev))
		}

		switch st.Kind() {
		case "":
			add(base, "step must have exactly one of: run, http")
		case KindRun:
			r := st.Run
			if (r.Script == "") == (len(r.Command) == 0) {
				add(base+"/run", "run needs exactly one of: script, command")
			}
			if r.Limits != nil && r.Limits.MemoryMB == 0 && r.Limits.CPUs == 0 && r.Limits.Pids == 0 {
				add(base+"/run/limits", "limits is empty")
			}
			checkTemplate(&issues, base+"/run/script", r.Script, names)
			for j, c := range r.Command {
				checkTemplate(&issues, fmt.Sprintf("%s/run/command/%d", base, j), c, names)
			}
			for k, v := range r.Env {
				checkTemplate(&issues, base+"/run/env/"+k, v, names)
			}
		case KindHTTP:
			h := st.HTTP
			checkTemplate(&issues, base+"/http/url", h.URL, names)
			for k, v := range h.Headers {
				checkTemplate(&issues, base+"/http/headers/"+k, v, names)
			}
			if s, ok := h.Body.(string); ok {
				checkTemplate(&issues, base+"/http/body", s, names)
			}
			for j, code := range h.ExpectStatus {
				if code < 100 || code > 599 {
					add(fmt.Sprintf("%s/http/expect_status/%d", base, j), "status code must be between 100 and 599")
				}
			}
			if h.Auth != "" && h.Auth != "none" && st.Connection == "" {
				add(base+"/http/auth", fmt.Sprintf("auth %q requires a step connection", h.Auth))
			}
		}

		for k, v := range st.Env {
			checkTemplate(&issues, base+"/env/"+k, v, names)
		}
		if st.If != "" {
			checkTemplate(&issues, base+"/if", st.If, names)
		}
		if st.Timeout != "" {
			d, err := time.ParseDuration(st.Timeout)
			switch {
			case err != nil || d <= 0:
				add(base+"/timeout", "must be a positive Go duration such as 10m")
			case opts.MaxTimeout > 0 && d > opts.MaxTimeout:
				add(base+"/timeout", fmt.Sprintf("exceeds the server maximum of %s", opts.MaxTimeout))
			}
		}
		if st.Retry != nil && st.Retry.Backoff != "" {
			if d, err := time.ParseDuration(st.Retry.Backoff); err != nil || d < 0 {
				add(base+"/retry/backoff", "must be a Go duration such as 10s")
			}
		}
		if st.Connection != "" && opts.ConnectionExists != nil && !opts.ConnectionExists(st.Connection) {
			add(base+"/connection", fmt.Sprintf("unknown connection %q", st.Connection))
		}

		// Register after checks so a step cannot reference itself.
		names[st.Name] = i
	}
	return issues
}

// checkTemplate parses a templated string and verifies step references point
// at steps that run earlier.
func checkTemplate(issues *Issues, path, text string, earlier map[string]int) {
	if !HasActions(text) {
		return
	}
	t, err := ParseTemplate(path, text)
	if err != nil {
		msg := strings.TrimPrefix(err.Error(), "template: "+path+":")
		*issues = append(*issues, Issue{Path: path, Message: "template error: " + strings.TrimSpace(msg)})
		return
	}
	for _, ref := range StepRefs(t) {
		if _, ok := earlier[ref]; ok {
			continue
		}
		is := Issue{Path: path, Message: fmt.Sprintf("references steps.%s, which is not an earlier step", ref)}
		if hint := nearest(ref, keys(earlier)); hint != "" {
			is.Hint = "did you mean steps." + hint + "?"
		}
		*issues = append(*issues, is)
	}
}

// schemaIssues flattens a validation error tree into leaf issues.
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
		if len(e.InstanceLocation) == 0 {
			path = "/"
		}
		is := Issue{Path: path, Message: e.ErrorKind.LocalizedString(printer)}
		switch k := e.ErrorKind.(type) {
		case *kind.AdditionalProperties:
			known := knownKeysAt(e.InstanceLocation)
			for _, p := range k.Properties {
				is := Issue{Path: path + "/" + p, Message: fmt.Sprintf("unknown field %q", p)}
				if hint := nearest(p, known); hint != "" {
					is.Hint = fmt.Sprintf("did you mean %q?", hint)
				}
				out = append(out, is)
			}
			return
		case *kind.Required:
			is.Message = fmt.Sprintf("missing required field(s): %s", strings.Join(k.Missing, ", "))
		case *kind.Enum:
			is.Message = "value is not one of the allowed values"
			if len(k.Want) > 0 {
				want := make([]string, len(k.Want))
				for i, v := range k.Want {
					want[i] = fmt.Sprint(v)
				}
				is.Hint = "allowed: " + strings.Join(want, ", ")
				if gs, ok := k.Got.(string); ok {
					if h := nearest(gs, want); h != "" {
						is.Hint = fmt.Sprintf("did you mean %q? allowed: %s", h, strings.Join(want, ", "))
					}
				}
			}
		}
		out = append(out, is)
	}
	walk(ve)
	return out
}

// knownKeysAt walks the Go spec types along an instance path and returns the
// JSON field names valid at that location.
func knownKeysAt(loc []string) []string {
	t := reflect.TypeOf(Workflow{})
	for _, seg := range loc {
		for t.Kind() == reflect.Ptr {
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
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
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

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}

// nearest returns the candidate closest to s when it is close enough to be a
// plausible typo, else "".
func nearest(s string, candidates []string) string {
	best, bestDist := "", -1
	for _, c := range candidates {
		d := levenshtein(strings.ToLower(s), strings.ToLower(c))
		if bestDist == -1 || d < bestDist {
			best, bestDist = c, d
		}
	}
	limit := 2
	if len(s) > 8 {
		limit = 3
	}
	if bestDist == -1 || bestDist > limit {
		return ""
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normalize converts yaml.v3 decoded values into JSON-compatible ones
// (map[string]any keys, no map[any]any) so they can be schema-validated.
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
	default:
		return v
	}
}
