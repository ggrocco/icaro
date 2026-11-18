// Package workflow defines the workflow contract (spec types, the
// generated JSON Schema, structured validation, the DAG plan and the
// output-capture grammar). The engine that executes it lives in
// internal/workflow/engine.
package workflow

import "time"

// Step kinds.
const (
	KindImage    = "image"
	KindUses     = "uses"
	KindApproval = "approval"
	KindWait     = "wait"
	KindAwait    = "await"
)

// Kind returns the step form, or "" when none or more than one is set.
func (s *StepSpec) Kind() string {
	kind, n := "", 0
	for _, f := range []struct {
		set  bool
		kind string
	}{{s.Image != "", KindImage}, {s.Uses != "", KindUses}, {s.Approval != nil, KindApproval}, {s.Wait != "", KindWait}, {s.Await != nil, KindAwait}} {
		if f.set {
			kind, n = f.kind, n+1
		}
	}
	if n != 1 {
		return ""
	}
	return kind
}

// Pauses reports whether the step runs no container or action: it waits
// for a person (approval), for time to pass (wait) or for an integration
// event (await), and its run is parked meanwhile (ADRs 0030, 0032).
func (s *StepSpec) Pauses() bool {
	k := s.Kind()
	return k == KindApproval || k == KindWait || k == KindAwait
}

// AwaitOutputs are the outputs an await step adds to its event's payload
// fields: the delivery that resumed it ("manual:<id>" when a person did).
var AwaitOutputs = []string{"delivery_id"}

// ApprovalOutputs are the outputs an approval step produces.
var ApprovalOutputs = []string{"approved_by", "comment"}

// MaxWait caps a wait step.
const MaxWait = 30 * 24 * time.Hour

// Step returns the step with id, or nil.
func (s *Spec) Step(id string) *StepSpec {
	for i := range s.Steps {
		if s.Steps[i].ID == id {
			return &s.Steps[i]
		}
	}
	return nil
}

// StepIDs returns step ids in declaration order.
func (s *Spec) StepIDs() []string {
	out := make([]string, len(s.Steps))
	for i, st := range s.Steps {
		out[i] = st.ID
	}
	return out
}

// ConcurrencyPolicy returns the effective policy (default allow).
func (s *Spec) ConcurrencyPolicy() string {
	if s.Concurrency == "" {
		return ConcurrencyAllow
	}
	return s.Concurrency
}

// CronTriggers returns the cron expressions of the workflow's cron triggers.
func (s *Spec) CronTriggers() []string {
	var out []string
	for _, t := range s.Triggers {
		if t.Type == TriggerCron {
			out = append(out, t.Cron)
		}
	}
	return out
}

// IntegrationTriggers returns the workflow's integration triggers.
func (s *Spec) IntegrationTriggers() []TriggerSpec {
	var out []TriggerSpec
	for _, t := range s.Triggers {
		if t.Integration != "" {
			out = append(out, t)
		}
	}
	return out
}
