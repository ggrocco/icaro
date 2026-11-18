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
	KindAgent    = "agent"
	KindCall     = "call"
	KindAwait    = "await"
)

// Kind returns the step form, or "" when none or more than one is set.
func (s *StepSpec) Kind() string {
	kind, n := "", 0
	for _, f := range []struct {
		set  bool
		kind string
	}{{s.Image != "", KindImage}, {s.Uses != "", KindUses}, {s.Approval != nil, KindApproval}, {s.Wait != "", KindWait}, {s.Agent != nil, KindAgent}, {s.Call != nil, KindCall}, {s.Await != nil, KindAwait}} {
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
// for a person (approval), for time to pass (wait), for a child run
// (call) or for an integration event (await), and its run is parked
// meanwhile (ADRs 0030, 0032, 0038).
func (s *StepSpec) Pauses() bool {
	k := s.Kind()
	return k == KindApproval || k == KindWait || k == KindCall || k == KindAwait
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

// Callable reports whether the workflow has a call trigger (ADR 0038).
func (s *Spec) Callable() bool {
	for _, t := range s.Triggers {
		if t.Type == TriggerCall {
			return true
		}
	}
	return false
}

// Calls returns the names of the workflows the spec's call steps call, in
// step order, without duplicates.
func (s *Spec) Calls() []string {
	var out []string
	for _, st := range s.Steps {
		if st.Call != nil && !contains(out, st.Call.Workflow) {
			out = append(out, st.Call.Workflow)
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

// GenericWebhook is the integration whose events arrive on a per-workflow
// URL token (ADR 0015); its await steps correlate only with events
// delivered on their own workflow's token (ADR 0032).
const GenericWebhook = "webhook"

// UsesGenericWebhook reports whether the workflow is triggered by, or
// awaits, the generic webhook integration, and so needs a URL token.
func (s *Spec) UsesGenericWebhook() bool {
	for _, t := range s.Triggers {
		if t.Integration == GenericWebhook {
			return true
		}
	}
	for _, st := range s.Steps {
		if st.Await != nil && st.Await.Integration == GenericWebhook {
			return true
		}
	}
	return false
}
