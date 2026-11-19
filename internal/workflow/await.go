package workflow

import (
	"errors"

	"icaro/internal/expr"
)

// Correlator decides whether an integration event resumes an await step
// (ADR 0032): the step's match, evaluated over the event's payload under
// the root `event` and rendered as a string, must equal the step's
// rendered key, and its filter, when set, must hold. Parsed expressions
// are cached, so checking one delivery against many waiting steps parses
// each distinct expression once. A Correlator is owned by one goroutine.
type Correlator struct {
	cache map[string]*expr.Expr
}

// NewCorrelator returns an empty Correlator.
func NewCorrelator() *Correlator { return &Correlator{cache: map[string]*expr.Expr{}} }

func (c *Correlator) parse(src string) (*expr.Expr, error) {
	if e, ok := c.cache[src]; ok {
		return e, nil
	}
	e, err := expr.ParseCondition(src)
	if err != nil {
		return nil, err
	}
	c.cache[src] = e
	return e, nil
}

// Correlates reports whether payload resumes a step waiting with key. A
// field the payload lacks makes the event not correlate; any other
// evaluation error is returned.
func (c *Correlator) Correlates(match, filter, key string, payload map[string]any) (bool, error) {
	env := expr.Env{"event": payload}
	m, err := c.parse(match)
	if err != nil {
		return false, err
	}
	v, err := m.Eval(env)
	if err != nil {
		return false, missingIsFalse(err)
	}
	if expr.Stringify(v) != key {
		return false, nil
	}
	if filter == "" {
		return true, nil
	}
	f, err := c.parse(filter)
	if err != nil {
		return false, err
	}
	ok, err := f.EvalBool(env)
	if err != nil {
		return false, missingIsFalse(err)
	}
	return ok, nil
}

func missingIsFalse(err error) error {
	var mk *expr.MissingKeyError
	if errors.As(err, &mk) {
		return nil
	}
	return err
}

// AwaitOutputValues are the outputs of a matched await step: every
// payload field rendered as ${{ }} renders it, plus delivery_id.
func AwaitOutputValues(payload map[string]any, deliveryID string) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = expr.Stringify(v)
	}
	out["delivery_id"] = deliveryID
	return out
}
