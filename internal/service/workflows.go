package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"icaro/internal/store"
	"icaro/internal/workflow"
)

// WorkflowInfo is the API view of a workflow.
type WorkflowInfo struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Version     int                    `json:"version"`
	Status      string                 `json:"status"`
	Concurrency string                 `json:"concurrency"`
	Triggers    []workflow.TriggerSpec `json:"triggers,omitempty"`
	Steps       []string               `json:"steps"`
	// TriggerFields are the trigger.* keys the workflow reads; manual
	// runs must supply the required ones.
	TriggerFields TriggerFields `json:"trigger_fields"`
	// Callable is true when the workflow has a call trigger; Outputs are
	// its workflow output names, and Callers the workflows that call it
	// (ADR 0038, on a single workflow only).
	Callable bool     `json:"callable"`
	Outputs  []string `json:"outputs"`
	Callers  []string `json:"callers,omitempty"`
	// Warnings are what ApplyWorkflow found valid but worth a second
	// look, such as the callers the new version breaks (ADR 0038).
	Warnings  workflow.Issues `json:"warnings,omitempty"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	YAML      string          `json:"yaml,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"`
}

// TriggerFields lists referenced trigger payload keys.
type TriggerFields struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

// VersionInfo is one historical version.
type VersionInfo struct {
	Version   int    `json:"version"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
	YAML      string `json:"yaml,omitempty"`
}

const tsFormat = "2006-01-02T15:04:05Z"

func (s *Service) validateOptions(ctx context.Context) workflow.Options {
	opts := workflow.Options{MaxTimeout: s.maxTimeout, Catalog: s.catalog, Agents: s.agents}
	if s.store != nil {
		if cat, err := s.workflowCatalog(ctx); err == nil {
			opts.Workflows = cat
		} else {
			s.log.Warn("list workflows for call checks", "err", err)
		}
	}
	if s.secrets != nil {
		if names, err := s.secrets.Names(ctx); err == nil {
			set := map[string]bool{}
			for _, n := range names {
				set[n] = true
			}
			opts.SecretExists = func(n string) bool { return set[n] }
			opts.SecretBound = func(n string) bool {
				hosts, err := s.secrets.Hosts(ctx, n)
				return err == nil && len(hosts) > 0
			}
		}
	}
	return opts
}

// Warnings are what is valid in spec but worth a second look: an agent
// prompt that carries untrusted trigger text (ADR 0037), and what saving
// spec would break in the workflows that call it (ADR 0038).
func (s *Service) Warnings(ctx context.Context, spec *workflow.Spec) workflow.Issues {
	opts := s.validateOptions(ctx)
	out := append(workflow.Warnings(spec, opts), workflow.CallerWarnings(spec, opts)...)
	return append(out, s.awaitWarnings(spec)...)
}

// awaitWarnings flag await steps on a daemon with no daemon.public_url,
// which no provider can reach (ADR 0032).
func (s *Service) awaitWarnings(spec *workflow.Spec) workflow.Issues {
	if spec == nil || s.publicURL != "" {
		return nil
	}
	var out workflow.Issues
	for i, st := range spec.Steps {
		if st.Await == nil {
			continue
		}
		out = append(out, workflow.Issue{
			Path:    fmt.Sprintf("/steps/%d/await", i),
			Message: fmt.Sprintf("step %s awaits %s.%s, but daemon.public_url is unset, so no provider can reach this daemon", st.ID, st.Await.Integration, st.Await.Event),
			Hint:    "set daemon.public_url, or resolve the step by hand with icaro runs deliver; otherwise it fails at its deadline",
		})
	}
	return out
}

// workflowCatalog resolves call steps against the current version of
// every stored workflow (ADR 0038).
func (s *Service) workflowCatalog(ctx context.Context) (workflowSpecs, error) {
	ws, err := s.store.ListWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	out := workflowSpecs{}
	for _, w := range ws {
		if spec, err := decodeSpec(w.SpecJSON); err == nil {
			out[w.Name] = spec
		}
	}
	return out, nil
}

// workflowSpecs is a workflow.WorkflowCatalog over decoded specs.
type workflowSpecs map[string]*workflow.Spec

func (w workflowSpecs) Workflow(name string) (*workflow.Spec, bool) {
	spec, ok := w[name]
	return spec, ok
}

func (w workflowSpecs) WorkflowNames() []string {
	out := make([]string, 0, len(w))
	for n := range w {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Callers lists the workflows whose current version calls name, sorted.
func (s *Service) Callers(ctx context.Context, name string) ([]string, error) {
	cat, err := s.workflowCatalog(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, n := range cat.WorkflowNames() {
		if slices.Contains(cat[n].Calls(), name) {
			out = append(out, n)
		}
	}
	return out, nil
}

// AgentProfiles lists the profiles agent steps may use, in config order.
func (s *Service) AgentProfiles() []workflow.AgentProfile {
	return append([]workflow.AgentProfile{}, s.agents.Profiles...)
}

// ValidateWorkflow parses and validates YAML without storing it. Decode
// errors are reported as an issue at "/".
func (s *Service) ValidateWorkflow(ctx context.Context, yaml string) (*workflow.Spec, workflow.Issues, error) {
	spec, issues, err := workflow.Parse([]byte(yaml), s.validateOptions(ctx))
	if err != nil {
		return nil, workflow.Issues{{Path: "/", Message: err.Error()}}, nil
	}
	return spec, issues, nil
}

// ApplyWorkflow validates and stores a workflow as a new version.
func (s *Service) ApplyWorkflow(ctx context.Context, yaml, actor string) (*WorkflowInfo, error) {
	spec, issues, err := s.ValidateWorkflow(ctx, yaml)
	if err != nil {
		return nil, err
	}
	if len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	// Against the versions stored before this one: what the save breaks.
	warnings := s.Warnings(ctx, spec)
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	w, err := s.store.ApplyWorkflow(ctx, spec.Name, yaml, string(specJSON), actor)
	if err != nil {
		return nil, err
	}
	s.log.Info("workflow applied", "name", w.Name, "version", w.Version, "actor", actor)
	s.changed()
	info := s.info(w, spec, true)
	info.Warnings = warnings
	return info, nil
}

func (s *Service) lookup(ctx context.Context, name string) (*store.Workflow, error) {
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	return w, err
}

// GetWorkflow returns a workflow with its YAML and its callers.
func (s *Service) GetWorkflow(ctx context.Context, name string) (*WorkflowInfo, error) {
	w, err := s.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	info := s.info(w, nil, true)
	if info.Callers, err = s.Callers(ctx, w.Name); err != nil {
		return nil, err
	}
	return info, nil
}

// ListWorkflows returns summaries.
func (s *Service) ListWorkflows(ctx context.Context) ([]*WorkflowInfo, error) {
	ws, err := s.store.ListWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*WorkflowInfo, 0, len(ws))
	for _, w := range ws {
		out = append(out, s.info(w, nil, false))
	}
	return out, nil
}

// ActiveSpecs returns the specs of all active workflows (trigger routing
// and the scheduler).
func (s *Service) ActiveSpecs(ctx context.Context) ([]*workflow.Spec, error) {
	ws, err := s.store.ActiveWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*workflow.Spec, 0, len(ws))
	for _, w := range ws {
		spec, err := decodeSpec(w.SpecJSON)
		if err != nil {
			s.log.Warn("skip workflow with undecodable spec", "workflow", w.Name, "err", err)
			continue
		}
		out = append(out, spec)
	}
	return out, nil
}

// DeleteWorkflow removes a workflow definition (runs are kept).
func (s *Service) DeleteWorkflow(ctx context.Context, name string) error {
	w, err := s.lookup(ctx, name)
	if err != nil {
		return err
	}
	active, err := s.store.DeleteWorkflow(ctx, w.ID)
	if err != nil {
		return err
	}
	// Its queued runs are cancelled with it; running and parked ones are
	// stopped. A run's version survives the delete, so history stays
	// readable.
	for _, r := range active {
		s.stopActive(r)
		s.stopDescendants(ctx, r.ID)
	}
	s.log.Info("workflow deleted", "name", w.Name, "cancelled_runs", len(active))
	s.changed()
	return nil
}

// SetWorkflowStatus enables or disables a workflow.
func (s *Service) SetWorkflowStatus(ctx context.Context, name, status string) error {
	if status != store.WorkflowActive && status != store.WorkflowDisabled {
		return badRequest("status must be active or disabled")
	}
	w, err := s.lookup(ctx, name)
	if err != nil {
		return err
	}
	if err := s.store.SetWorkflowStatus(ctx, w.ID, status); err != nil {
		return err
	}
	s.changed()
	return nil
}

// ListWorkflowVersions returns version history, newest first.
func (s *Service) ListWorkflowVersions(ctx context.Context, name string) ([]VersionInfo, error) {
	w, err := s.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	vs, err := s.store.ListWorkflowVersions(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	out := make([]VersionInfo, 0, len(vs))
	for _, v := range vs {
		out = append(out, VersionInfo{Version: v.Version, Actor: v.Actor, CreatedAt: v.CreatedAt.UTC().Format(tsFormat)})
	}
	return out, nil
}

// GetWorkflowVersion returns one version with its YAML.
func (s *Service) GetWorkflowVersion(ctx context.Context, name string, version int) (*VersionInfo, error) {
	w, err := s.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	v, err := s.store.GetWorkflowVersion(ctx, w.ID, version)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &VersionInfo{Version: v.Version, Actor: v.Actor, CreatedAt: v.CreatedAt.UTC().Format(tsFormat), YAML: v.SpecYAML}, nil
}

func decodeSpec(specJSON string) (*workflow.Spec, error) {
	var spec workflow.Spec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func (s *Service) info(w *store.Workflow, spec *workflow.Spec, full bool) *WorkflowInfo {
	if spec == nil {
		spec, _ = decodeSpec(w.SpecJSON)
		if spec == nil {
			spec = &workflow.Spec{}
		}
	}
	req, opt := workflow.TriggerFields(spec)
	info := &WorkflowInfo{
		Name: w.Name, Description: spec.Description, Version: w.Version, Status: w.Status,
		Concurrency: spec.ConcurrencyPolicy(), Triggers: spec.Triggers, Steps: spec.StepIDs(),
		TriggerFields: TriggerFields{Required: orEmpty(req), Optional: orEmpty(opt)},
		Callable:      spec.Callable(), Outputs: outputNames(spec),
		CreatedAt: w.CreatedAt.UTC().Format(tsFormat), UpdatedAt: w.UpdatedAt.UTC().Format(tsFormat),
	}
	if full {
		info.YAML = w.SpecYAML
		info.Spec = json.RawMessage(w.SpecJSON)
	}
	return info
}

func outputNames(spec *workflow.Spec) []string {
	out := make([]string, 0, len(spec.Outputs))
	for k := range spec.Outputs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func tsp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}
