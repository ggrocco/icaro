package service

import (
	"context"
	"encoding/json"
	"errors"
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
	TriggerFields TriggerFields   `json:"trigger_fields"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	YAML          string          `json:"yaml,omitempty"`
	Spec          json.RawMessage `json:"spec,omitempty"`
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
	opts := workflow.Options{MaxTimeout: s.maxTimeout, Catalog: s.catalog}
	if s.secrets != nil {
		if names, err := s.secrets.Names(ctx); err == nil {
			set := map[string]bool{}
			for _, n := range names {
				set[n] = true
			}
			opts.SecretExists = func(n string) bool { return set[n] }
		}
	}
	return opts
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
	return s.info(w, spec, true), nil
}

func (s *Service) lookup(ctx context.Context, name string) (*store.Workflow, error) {
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	return w, err
}

// GetWorkflow returns a workflow with its YAML.
func (s *Service) GetWorkflow(ctx context.Context, name string) (*WorkflowInfo, error) {
	w, err := s.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.info(w, nil, true), nil
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
	if err := s.store.DeleteWorkflow(ctx, w.ID); err != nil {
		return err
	}
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
		CreatedAt:     w.CreatedAt.UTC().Format(tsFormat), UpdatedAt: w.UpdatedAt.UTC().Format(tsFormat),
	}
	if full {
		info.YAML = w.SpecYAML
		info.Spec = json.RawMessage(w.SpecJSON)
	}
	return info
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
