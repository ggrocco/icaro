package workflow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
)

// Roots are the top-level names templates can reference without a leading
// dot: {{ inputs.email }}, {{ steps.enrich.outputs.plan }}, {{ run.id }},
// {{ workflow.name }}. They are bound as zero-argument functions per render.
var Roots = []string{"inputs", "steps", "run", "workflow"}

// Funcs is the allow-listed function set available in templates. No file,
// env or exec access: templates only shape data that is already in the run.
// Root names resolve to empty maps at parse time; Render rebinds them.
func Funcs() template.FuncMap {
	fm := rootFuncs(nil)
	fm["json"] = func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	}
	fm["default"] = func(def, v any) any {
		if v == nil || v == "" {
			return def
		}
		return v
	}
	fm["upper"] = strings.ToUpper
	fm["lower"] = strings.ToLower
	fm["trim"] = strings.TrimSpace
	fm["quote"] = strconv.Quote
	fm["join"] = func(sep string, v []any) string {
		parts := make([]string, len(v))
		for i, p := range v {
			parts[i] = fmt.Sprint(p)
		}
		return strings.Join(parts, sep)
	}
	return fm
}

// rootFuncs binds each root name to a function returning its value in data.
// A nil value is normalised to an empty map so field access on an absent
// root reports a missing key instead of a nil dereference.
func rootFuncs(data map[string]any) template.FuncMap {
	fm := template.FuncMap{}
	for _, root := range Roots {
		v := data[root]
		if v == nil {
			v = map[string]any{}
		}
		fm[root] = func() any { return v }
	}
	return fm
}

// ParseTemplate parses text with the restricted function set. Missing keys
// are errors at render time so typos never silently render as empty.
func ParseTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Funcs(Funcs()).Parse(text)
}

// Render executes a parsed template with the root names bound to data.
func Render(t *template.Template, data map[string]any) (string, error) {
	var sb strings.Builder
	if err := t.Funcs(rootFuncs(data)).Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// RenderString parses and renders in one call.
func RenderString(name, text string, data map[string]any) (string, error) {
	t, err := ParseTemplate(name, text)
	if err != nil {
		return "", err
	}
	return Render(t, data)
}

// RenderBool renders an `if:` template and parses the trimmed result as a
// boolean. An empty template means true.
func RenderBool(name, text string, data map[string]any) (bool, error) {
	if strings.TrimSpace(text) == "" {
		return true, nil
	}
	out, err := RenderString(name, text, data)
	if err != nil {
		return false, err
	}
	b, err := strconv.ParseBool(strings.TrimSpace(out))
	if err != nil {
		return false, fmt.Errorf("if: rendered %q, want true or false", strings.TrimSpace(out))
	}
	return b, nil
}

// HasActions reports whether the text contains any template actions.
func HasActions(text string) bool { return strings.Contains(text, "{{") }

// StepRefs returns the step names referenced as steps.<name> in a template.
func StepRefs(t *template.Template) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(ident []string) {
		if len(ident) >= 2 && ident[0] == "steps" && !seen[ident[1]] {
			seen[ident[1]] = true
			refs = append(refs, ident[1])
		}
	}
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch n := n.(type) {
		case *parse.ListNode:
			if n == nil {
				return
			}
			for _, c := range n.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(n.Pipe)
		case *parse.IfNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.RangeNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.WithNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.PipeNode:
			if n == nil {
				return
			}
			for _, c := range n.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, a := range n.Args {
				walk(a)
			}
		case *parse.FieldNode:
			add(n.Ident)
		case *parse.ChainNode:
			if id, ok := n.Node.(*parse.IdentifierNode); ok {
				add(append([]string{id.Ident}, n.Field...))
			} else {
				walk(n.Node)
			}
		}
	}
	if t.Tree != nil {
		walk(t.Root)
	}
	return refs
}
