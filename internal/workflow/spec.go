// Package workflow defines the workflow contract (spec types, the
// generated JSON Schema, structured validation, the DAG plan and the
// output-capture grammar). The engine that executes it lives in
// internal/workflow/engine.
package workflow

// Step kinds.
const (
	KindImage = "image"
	KindUses  = "uses"
)

// Kind returns the step form, or "" when neither or both are set.
func (s *StepSpec) Kind() string {
	switch {
	case s.Image != "" && s.Uses == "":
		return KindImage
	case s.Uses != "" && s.Image == "":
		return KindUses
	}
	return ""
}

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
