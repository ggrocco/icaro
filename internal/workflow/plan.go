package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// Plan is the validated DAG of a workflow.
type Plan struct {
	// Order is a topological order, stable with respect to declaration.
	Order []string
	// Needs maps a step to its direct dependencies.
	Needs map[string][]string
	// Dependents maps a step to the steps that need it directly.
	Dependents map[string][]string
	// Ancestors maps a step to all of its transitive dependencies.
	Ancestors map[string]map[string]bool
}

// CycleError reports a dependency cycle.
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	return "dependency cycle: " + strings.Join(e.Path, " -> ")
}

// NewPlan builds the DAG. It fails on unknown needs and on cycles.
func NewPlan(s *Spec) (*Plan, error) {
	p := &Plan{Needs: map[string][]string{}, Dependents: map[string][]string{}, Ancestors: map[string]map[string]bool{}}
	index := map[string]int{}
	for i, st := range s.Steps {
		index[st.ID] = i
	}
	for _, st := range s.Steps {
		for _, n := range st.Needs {
			if _, ok := index[n]; !ok {
				return nil, fmt.Errorf("step %s needs unknown step %s", st.ID, n)
			}
		}
		p.Needs[st.ID] = append([]string(nil), st.Needs...)
		for _, n := range st.Needs {
			p.Dependents[n] = append(p.Dependents[n], st.ID)
		}
	}
	if cyc := findCycle(s, p.Needs); cyc != nil {
		return nil, &CycleError{Path: cyc}
	}

	// Kahn's algorithm, picking the earliest declared ready step each time.
	indeg := map[string]int{}
	for _, st := range s.Steps {
		indeg[st.ID] = len(dedupe(st.Needs))
	}
	done := map[string]bool{}
	for len(p.Order) < len(s.Steps) {
		next := ""
		for _, st := range s.Steps {
			if !done[st.ID] && indeg[st.ID] == 0 {
				next = st.ID
				break
			}
		}
		done[next] = true
		p.Order = append(p.Order, next)
		for _, d := range dedupe(p.Dependents[next]) {
			indeg[d]--
		}
	}
	for _, id := range p.Order {
		anc := map[string]bool{}
		for _, n := range p.Needs[id] {
			anc[n] = true
			for a := range p.Ancestors[n] {
				anc[a] = true
			}
		}
		p.Ancestors[id] = anc
	}
	return p, nil
}

// findCycle returns one cycle as a path a -> b -> a, or nil.
func findCycle(s *Spec, needs map[string][]string) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var found []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = grey
		stack = append(stack, id)
		for _, n := range needs[id] {
			switch color[n] {
			case grey:
				for i, sid := range stack {
					if sid == n {
						found = append(append([]string(nil), stack[i:]...), n)
						return true
					}
				}
			case white:
				if visit(n) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	for _, st := range s.Steps {
		if color[st.ID] == white && visit(st.ID) {
			// Report the cycle in dependency direction (a needs b => a -> b).
			return found
		}
	}
	return nil
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Roots returns steps with no needs, in order.
func (p *Plan) Roots() []string {
	var out []string
	for _, id := range p.Order {
		if len(p.Needs[id]) == 0 {
			out = append(out, id)
		}
	}
	return out
}

// Index returns each step's position in Order.
func (p *Plan) Index() map[string]int {
	out := make(map[string]int, len(p.Order))
	for i, id := range p.Order {
		out[id] = i
	}
	return out
}

// sortedKeys returns map keys sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
