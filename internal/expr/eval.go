package expr

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type node interface {
	eval(env Env) (any, error)
	refs(out *[]Ref, guarded bool)
}

type litNode struct{ v any }

func (n *litNode) eval(Env) (any, error) { return n.v, nil }
func (n *litNode) refs(*[]Ref, bool)     {}
func (n *rootNode) refs(out *[]Ref, g bool) {
	*out = append(*out, Ref{Path: []string{n.name}, Guarded: g})
}

type rootNode struct{ name string }

func (n *rootNode) eval(env Env) (any, error) {
	v, ok := env[n.name]
	if !ok {
		return nil, &MissingKeyError{Path: n.name}
	}
	return v, nil
}

type indexNode struct {
	base   node
	key    node
	static string // literal key, "" when dynamic
}

// chain unwinds a.b[x].c into its base node and index nodes, root first.
func (n *indexNode) chain() (node, []*indexNode) {
	var rev []*indexNode
	var cur node = n
	for {
		in, ok := cur.(*indexNode)
		if !ok {
			break
		}
		rev = append(rev, in)
		cur = in.base
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return cur, rev
}

// describe renders the reference for error messages.
func (n *indexNode) describe() string {
	base, chain := n.chain()
	var sb strings.Builder
	if r, ok := base.(*rootNode); ok {
		sb.WriteString(r.name)
	} else {
		sb.WriteString("(value)")
	}
	for _, in := range chain {
		if in.static != "" {
			sb.WriteString("." + in.static)
		} else {
			sb.WriteString("[...]")
		}
	}
	return sb.String()
}

func (n *indexNode) refs(out *[]Ref, g bool) {
	base, chain := n.chain()
	root, ok := base.(*rootNode)
	if !ok {
		base.refs(out, g)
		for _, in := range chain {
			in.key.refs(out, g)
		}
		return
	}
	ref := Ref{Path: []string{root.name}, Guarded: g}
	for _, in := range chain {
		if ref.Partial || in.static == "" {
			ref.Partial = true
			in.key.refs(out, g)
			continue
		}
		ref.Path = append(ref.Path, in.static)
	}
	*out = append(*out, ref)
}

func (n *indexNode) eval(env Env) (any, error) {
	base, err := n.base.eval(env)
	if err != nil {
		return nil, err
	}
	key, err := n.key.eval(env)
	if err != nil {
		return nil, err
	}
	return index(base, key, n.describe)
}

func index(base, key any, path func() string) (any, error) {
	switch b := base.(type) {
	case map[string]any:
		k, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("cannot index an object with %s", typeName(key))
		}
		v, ok := b[k]
		if !ok {
			return nil, &MissingKeyError{Path: path()}
		}
		return v, nil
	case map[string]string:
		k, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("cannot index an object with %s", typeName(key))
		}
		v, ok := b[k]
		if !ok {
			return nil, &MissingKeyError{Path: path()}
		}
		return v, nil
	case []any:
		f, ok := toNumber(key)
		if !ok || f != math.Trunc(f) {
			return nil, fmt.Errorf("cannot index an array with %s", typeName(key))
		}
		i := int(f)
		if i < 0 || i >= len(b) {
			return nil, &MissingKeyError{Path: path()}
		}
		return b[i], nil
	case nil:
		return nil, &MissingKeyError{Path: path()}
	default:
		return nil, fmt.Errorf("cannot index %s (at %s)", typeName(base), path())
	}
}

type hasNode struct{ ref node }

func (n *hasNode) eval(env Env) (any, error) {
	_, err := n.ref.eval(env)
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*MissingKeyError); ok {
		return false, nil
	}
	return nil, err
}
func (n *hasNode) refs(out *[]Ref, _ bool) { n.ref.refs(out, true) }

type notNode struct{ n node }

func (n *notNode) eval(env Env) (any, error) {
	v, err := n.n.eval(env)
	if err != nil {
		return nil, err
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("! needs a boolean, got %s", typeName(v))
	}
	return !b, nil
}
func (n *notNode) refs(out *[]Ref, g bool) { n.n.refs(out, g) }

type logicNode struct {
	op   string
	l, r node
}

func (n *logicNode) eval(env Env) (any, error) {
	lv, err := n.l.eval(env)
	if err != nil {
		return nil, err
	}
	lb, ok := lv.(bool)
	if !ok {
		return nil, fmt.Errorf("%s needs booleans, got %s", n.op, typeName(lv))
	}
	if n.op == "&&" && !lb {
		return false, nil
	}
	if n.op == "||" && lb {
		return true, nil
	}
	rv, err := n.r.eval(env)
	if err != nil {
		return nil, err
	}
	rb, ok := rv.(bool)
	if !ok {
		return nil, fmt.Errorf("%s needs booleans, got %s", n.op, typeName(rv))
	}
	return rb, nil
}

// refs marks references on the right of && (or ||) as guarded when the
// left side, evaluated first, has proven them present with has(): the
// right side only runs after `has(trigger.x) &&` is true, or after
// `!has(trigger.x) ||` is false. Short-circuit evaluation makes this
// exact, so `has(trigger.x) && trigger.x == 'y'` reads trigger.x only
// when it exists.
func (n *logicNode) refs(out *[]Ref, g bool) {
	n.l.refs(out, g)
	start := len(*out)
	n.r.refs(out, g)
	proven := present(n.l, n.op == "&&")
	for i := start; i < len(*out); i++ {
		if coveredBy((*out)[i].Path, proven) {
			(*out)[i].Guarded = true
		}
	}
}

// present returns the reference paths that n proves exist when it
// evaluates to want.
func present(n node, want bool) [][]string {
	switch x := n.(type) {
	case *hasNode:
		if p := staticPath(x.ref); want && p != nil {
			return [][]string{p}
		}
	case *notNode:
		return present(x.n, !want)
	case *logicNode:
		// a && b is true only when both are; a || b is false only when
		// both are. Other outcomes prove nothing about either side.
		if (x.op == "&&") == want {
			return append(present(x.l, want), present(x.r, want)...)
		}
	}
	return nil
}

// staticPath is the root and literal segments of a reference, up to the
// first dynamic index; nil when n is not rooted.
func staticPath(n node) []string {
	in, ok := n.(*indexNode)
	if !ok {
		return nil
	}
	base, chain := in.chain()
	root, ok := base.(*rootNode)
	if !ok {
		return nil
	}
	p := []string{root.name}
	for _, c := range chain {
		if c.static == "" {
			break
		}
		p = append(p, c.static)
	}
	return p
}

// coveredBy reports whether a has() of one of proven guards path: the
// same reference, a parent (has(trigger.label) && trigger.label.name) or
// a child (has(trigger.label.name) && trigger.label).
func coveredBy(path []string, proven [][]string) bool {
	for _, p := range proven {
		if isPrefix(p, path) || isPrefix(path, p) {
			return true
		}
	}
	return false
}

func isPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type cmpNode struct {
	op   string
	l, r node
	pos  int
}

func (n *cmpNode) refs(out *[]Ref, g bool) { n.l.refs(out, g); n.r.refs(out, g) }

func (n *cmpNode) eval(env Env) (any, error) {
	l, err := n.l.eval(env)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "==":
		return Equal(l, r), nil
	case "!=":
		return !Equal(l, r), nil
	}
	c, err := compare(l, r)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	default:
		return c >= 0, nil
	}
}

// Equal compares values with numbers normalized (1 == 1.0).
func Equal(a, b any) bool {
	if fa, ok := toNumber(a); ok {
		fb, ok := toNumber(b)
		return ok && fa == fb
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}

func compare(a, b any) (int, error) {
	if fa, ok := toNumber(a); ok {
		if fb, ok := toNumber(b); ok {
			switch {
			case fa < fb:
				return -1, nil
			case fa > fb:
				return 1, nil
			}
			return 0, nil
		}
	}
	sa, oka := a.(string)
	sb, okb := b.(string)
	if oka && okb {
		return strings.Compare(sa, sb), nil
	}
	return 0, fmt.Errorf("cannot order %s and %s", typeName(a), typeName(b))
}

func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = s
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = normalize(s)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = normalize(s)
		}
		return out
	}
	if f, ok := toNumber(v); ok {
		return f
	}
	return v
}

type callNode struct {
	name string
	fn   function
	args []node
}

func (n *callNode) refs(out *[]Ref, g bool) {
	for _, a := range n.args {
		a.refs(out, g)
	}
}

func (n *callNode) eval(env Env) (any, error) {
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(env)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	v, err := n.fn.call(args)
	if err != nil {
		return nil, fmt.Errorf("%s(): %w", n.name, err)
	}
	return v, nil
}

type function struct {
	min, max int
	call     func(args []any) (any, error)
}

func (f function) arity() string {
	switch {
	case f.min == f.max:
		return strconv.Itoa(f.min)
	case f.max < 0:
		return fmt.Sprintf("at least %d", f.min)
	}
	return fmt.Sprintf("%d-%d", f.min, f.max)
}

func str(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("want a string, got %s", typeName(v))
	}
	return s, nil
}

func str2(args []any, fn func(a, b string) any) (any, error) {
	a, err := str(args[0])
	if err != nil {
		return nil, err
	}
	b, err := str(args[1])
	if err != nil {
		return nil, err
	}
	return fn(a, b), nil
}

// funcs is the allow-list. No function touches the environment, files or
// processes.
var funcs = map[string]function{
	"contains": {2, 2, func(a []any) (any, error) {
		switch h := a[0].(type) {
		case string:
			n, err := str(a[1])
			if err != nil {
				return nil, err
			}
			return strings.Contains(strings.ToLower(h), strings.ToLower(n)), nil
		case []any:
			for _, x := range h {
				if Equal(x, a[1]) {
					return true, nil
				}
			}
			return false, nil
		}
		return nil, fmt.Errorf("want a string or array, got %s", typeName(a[0]))
	}},
	"startsWith": {2, 2, func(a []any) (any, error) {
		return str2(a, func(x, y string) any { return strings.HasPrefix(x, y) })
	}},
	"endsWith": {2, 2, func(a []any) (any, error) {
		return str2(a, func(x, y string) any { return strings.HasSuffix(x, y) })
	}},
	"lower": {1, 1, func(a []any) (any, error) {
		s, err := str(a[0])
		return strings.ToLower(s), err
	}},
	"upper": {1, 1, func(a []any) (any, error) {
		s, err := str(a[0])
		return strings.ToUpper(s), err
	}},
	"trim": {1, 1, func(a []any) (any, error) {
		s, err := str(a[0])
		return strings.TrimSpace(s), err
	}},
	"length": {1, 1, func(a []any) (any, error) {
		switch v := a[0].(type) {
		case string:
			return float64(len([]rune(v))), nil
		case []any:
			return float64(len(v)), nil
		case map[string]any:
			return float64(len(v)), nil
		}
		return nil, fmt.Errorf("want a string, array or object, got %s", typeName(a[0]))
	}},
	"toJSON": {1, 1, func(a []any) (any, error) {
		b, err := json.Marshal(a[0])
		return string(b), err
	}},
	"fromJSON": {1, 1, func(a []any) (any, error) {
		s, err := str(a[0])
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, err
		}
		return v, nil
	}},
	"join": {1, 2, func(a []any) (any, error) {
		arr, ok := a[0].([]any)
		if !ok {
			return nil, fmt.Errorf("want an array, got %s", typeName(a[0]))
		}
		sep := ","
		if len(a) == 2 {
			s, err := str(a[1])
			if err != nil {
				return nil, err
			}
			sep = s
		}
		parts := make([]string, len(arr))
		for i, x := range arr {
			parts[i] = Stringify(x)
		}
		return strings.Join(parts, sep), nil
	}},
	"split": {2, 2, func(a []any) (any, error) {
		return str2(a, func(x, y string) any {
			parts := strings.Split(x, y)
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return out
		})
	}},
	"format": {1, -1, func(a []any) (any, error) {
		f, err := str(a[0])
		if err != nil {
			return nil, err
		}
		for i, v := range a[1:] {
			f = strings.ReplaceAll(f, "{"+strconv.Itoa(i)+"}", Stringify(v))
		}
		return f, nil
	}},
	"coalesce": {1, -1, func(a []any) (any, error) {
		for _, v := range a {
			if v != nil && v != "" {
				return v, nil
			}
		}
		return nil, nil
	}},
	"string": {1, 1, func(a []any) (any, error) { return Stringify(a[0]), nil }},
	"number": {1, 1, func(a []any) (any, error) {
		if f, ok := toNumber(a[0]); ok {
			return f, nil
		}
		s, err := str(a[0])
		if err != nil {
			return nil, err
		}
		return strconv.ParseFloat(strings.TrimSpace(s), 64)
	}},
}

// FuncNames lists the allow-listed functions (plus has).
func FuncNames() []string {
	out := []string{"has"}
	for k := range funcs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Stringify renders a value for interpolation: strings as-is, integers
// without a decimal point, null as empty, everything else as JSON.
func Stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	}
	if f, ok := toNumber(v); ok {
		if f == math.Trunc(f) && math.Abs(f) < 1e15 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case map[string]any, map[string]string:
		return "an object"
	case []any:
		return "an array"
	}
	if _, ok := toNumber(v); ok {
		return "a number"
	}
	return fmt.Sprintf("%T", v)
}

// Nearest returns the candidate closest to s when it is a plausible typo.
func Nearest(s string, candidates []string) string {
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
	if bestDist == -1 || bestDist > limit || best == s {
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
