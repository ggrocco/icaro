// Package expr is the interpolation and condition language shared by
// workflows and integration manifests (ADR 0007, ADR 0009).
//
// A template is text with ${{ expression }} segments. A condition (`when`,
// `filter`, manifest `match`) is a bare expression, optionally wrapped in a
// single ${{ }}. Expressions are parsed at validation time, a missing key
// is an error, and functions come from a fixed allow-list with no env,
// file or exec access.
//
// Grammar:
//
//	expr    = or
//	or      = and { "||" and }
//	and     = cmp { "&&" cmp }
//	cmp     = unary [ ("=="|"!="|"<"|"<="|">"|">=") unary ]
//	unary   = "!" unary | postfix
//	postfix = primary { "." ident | "[" expr "]" }
//	primary = number | string | "true" | "false" | "null"
//	        | ident "(" [ expr { "," expr } ] ")"   (allow-listed function)
//	        | ident                                 (a root, e.g. steps)
//	        | "(" expr ")"
//
// Identifiers may contain letters, digits, '_' and '-', so step ids such
// as `build-app` work in paths. Strings use single or double quotes.
package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// Error is a parse or evaluation error with a byte offset into the source.
type Error struct {
	Src string
	Pos int
	Msg string
}

func (e *Error) Error() string {
	if e.Pos >= 0 {
		return fmt.Sprintf("%s (at offset %d in %q)", e.Msg, e.Pos, e.Src)
	}
	return e.Msg
}

// MissingKeyError reports a reference to an absent key.
type MissingKeyError struct {
	Path string
}

func (e *MissingKeyError) Error() string { return fmt.Sprintf("%s is not defined", e.Path) }

// Env maps root names (steps, trigger, env, ...) to their values.
type Env map[string]any

// Ref is a static reference found in an expression: the root followed by
// the literal path segments up to the first dynamic index.
type Ref struct {
	Path []string
	// Partial is true when a dynamic index follows Path.
	Partial bool
	// Guarded is true inside has(...), where absence is not an error.
	Guarded bool
}

func (r Ref) String() string { return strings.Join(r.Path, ".") }

// Expr is a parsed expression.
type Expr struct {
	src  string
	root node
}

// Parse parses a bare expression.
func Parse(src string) (*Expr, error) {
	p := &parser{src: src, toks: nil}
	if err := p.lex(); err != nil {
		return nil, err
	}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errAt(p.peek().pos, "unexpected %s", p.peek())
	}
	return &Expr{src: src, root: n}, nil
}

// ParseCondition parses a condition: a bare expression or one wrapped in
// a single ${{ }}.
func ParseCondition(src string) (*Expr, error) {
	s := strings.TrimSpace(src)
	if strings.HasPrefix(s, "${{") && strings.HasSuffix(s, "}}") && strings.Count(s, "${{") == 1 {
		s = strings.TrimSpace(s[3 : len(s)-2])
	}
	if s == "" {
		return nil, &Error{Src: src, Pos: 0, Msg: "empty expression"}
	}
	return Parse(s)
}

// String returns the source text.
func (e *Expr) String() string { return e.src }

// Eval evaluates the expression.
func (e *Expr) Eval(env Env) (any, error) { return e.root.eval(env) }

// EvalBool evaluates a condition, which must produce a boolean.
func (e *Expr) EvalBool(env Env) (bool, error) {
	v, err := e.Eval(env)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("condition %q produced %s, want a boolean", e.src, typeName(v))
	}
	return b, nil
}

// Refs returns the static references in the expression.
func (e *Expr) Refs() []Ref {
	var out []Ref
	e.root.refs(&out, false)
	return out
}

// Template is text with ${{ }} segments.
type Template struct {
	src   string
	parts []tpart
}

type tpart struct {
	lit  string
	expr *Expr
}

// Contains reports whether s has any ${{ }} segment.
func Contains(s string) bool { return strings.Contains(s, "${{") }

// ParseTemplate parses a template string.
func ParseTemplate(src string) (*Template, error) {
	t := &Template{src: src}
	rest := src
	off := 0
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			if rest != "" {
				t.parts = append(t.parts, tpart{lit: rest})
			}
			return t, nil
		}
		if i > 0 {
			t.parts = append(t.parts, tpart{lit: rest[:i]})
		}
		end := findClose(rest[i+3:])
		if end < 0 {
			return nil, &Error{Src: src, Pos: off + i, Msg: "unterminated ${{"}
		}
		inner := rest[i+3 : i+3+end]
		e, err := Parse(strings.TrimSpace(inner))
		if err != nil {
			if pe, ok := err.(*Error); ok {
				pe.Src, pe.Pos = src, off+i+3+pe.Pos
			}
			return nil, err
		}
		t.parts = append(t.parts, tpart{expr: e})
		consumed := i + 3 + end + 2
		off += consumed
		rest = rest[consumed:]
	}
}

// findClose finds the "}}" closing an expression, skipping quoted strings.
func findClose(s string) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '}' && i+1 < len(s) && s[i+1] == '}':
			return i
		}
	}
	return -1
}

// IsLiteral reports whether the template has no expressions.
func (t *Template) IsLiteral() bool {
	for _, p := range t.parts {
		if p.expr != nil {
			return false
		}
	}
	return true
}

// Eval evaluates the template. A template that is exactly one ${{ }}
// keeps the value's type; anything else renders to a string.
func (t *Template) Eval(env Env) (any, error) {
	if len(t.parts) == 1 && t.parts[0].expr != nil {
		return t.parts[0].expr.Eval(env)
	}
	return t.Render(env)
}

// Render evaluates the template to a string. Non-string values render as
// JSON (strings inside objects stay quoted).
func (t *Template) Render(env Env) (string, error) {
	var sb strings.Builder
	for _, p := range t.parts {
		if p.expr == nil {
			sb.WriteString(p.lit)
			continue
		}
		v, err := p.expr.Eval(env)
		if err != nil {
			return "", err
		}
		sb.WriteString(Stringify(v))
	}
	return sb.String(), nil
}

// Refs returns the references of every expression in the template.
func (t *Template) Refs() []Ref {
	var out []Ref
	for _, p := range t.parts {
		if p.expr != nil {
			out = append(out, p.expr.Refs()...)
		}
	}
	return out
}

// Exprs returns the parsed expressions.
func (t *Template) Exprs() []*Expr {
	var out []*Expr
	for _, p := range t.parts {
		if p.expr != nil {
			out = append(out, p.expr)
		}
	}
	return out
}

// RenderString parses and renders a template in one call.
func RenderString(src string, env Env) (string, error) {
	if !Contains(src) {
		return src, nil
	}
	t, err := ParseTemplate(src)
	if err != nil {
		return "", err
	}
	return t.Render(env)
}

// EvalValue walks a JSON-like value and evaluates every string as a
// template; whole-expression strings keep their type.
func EvalValue(v any, env Env) (any, error) {
	switch v := v.(type) {
	case string:
		if !Contains(v) {
			return v, nil
		}
		t, err := ParseTemplate(v)
		if err != nil {
			return nil, err
		}
		return t.Eval(env)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			r, err := EvalValue(val, env)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			r, err := EvalValue(val, env)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// WalkTemplates calls fn for every string in a JSON-like value with its
// JSON Pointer suffix (e.g. "/a/0/b").
func WalkTemplates(v any, fn func(ptr, s string)) {
	var walk func(ptr string, v any)
	walk = func(ptr string, v any) {
		switch v := v.(type) {
		case string:
			fn(ptr, v)
		case map[string]any:
			for k, val := range v {
				walk(ptr+"/"+escapePtr(k), val)
			}
		case []any:
			for i, val := range v {
				walk(ptr+"/"+strconv.Itoa(i), val)
			}
		}
	}
	walk("", v)
}

func escapePtr(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// CheckRoots returns an error for the first reference whose root is not
// in allowed, with a nearest-match hint.
func CheckRoots(refs []Ref, allowed []string) error {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, r := range refs {
		if len(r.Path) == 0 || ok[r.Path[0]] {
			continue
		}
		msg := fmt.Sprintf("unknown name %q (available: %s)", r.Path[0], strings.Join(allowed, ", "))
		if h := Nearest(r.Path[0], allowed); h != "" {
			msg += fmt.Sprintf("; did you mean %q?", h)
		}
		return &Error{Pos: -1, Msg: msg}
	}
	return nil
}
