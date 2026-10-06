package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"

	"icaro/internal/store"
	"icaro/internal/workflow"
)

// WorkflowInfo is the API view of a workflow.
type WorkflowInfo struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Version      int             `json:"version"`
	Status       string          `json:"status"`
	WebhookToken string          `json:"webhook_token,omitempty"`
	Schedules    []string        `json:"schedules,omitempty"`
	Steps        []string        `json:"steps"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
	YAML         string          `json:"yaml,omitempty"`
	Spec         json.RawMessage `json:"spec,omitempty"`
}

// VersionInfo is one historical version.
type VersionInfo struct {
	Version   int    `json:"version"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
	YAML      string `json:"yaml,omitempty"`
}

func (s *Service) validateOptions() workflow.Options {
	return workflow.Options{
		MaxTimeout: s.maxTimeout,
		ConnectionExists: func(name string) bool {
			_, err := s.store.GetConnection(context.Background(), name)
			return err == nil
		},
	}
}

// ValidateWorkflow parses and validates YAML without storing it.
func (s *Service) ValidateWorkflow(_ context.Context, yaml string) (*workflow.Workflow, workflow.Issues, error) {
	wf, issues, err := workflow.Parse([]byte(yaml), s.validateOptions())
	if err != nil {
		return nil, workflow.Issues{{Path: "/", Message: err.Error()}}, nil
	}
	return wf, issues, nil
}

// ApplyWorkflow validates and stores a workflow as a new version.
func (s *Service) ApplyWorkflow(ctx context.Context, yaml, actor string) (*WorkflowInfo, error) {
	wf, issues, err := s.ValidateWorkflow(ctx, yaml)
	if err != nil {
		return nil, err
	}
	if len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	specJSON, err := json.Marshal(wf)
	if err != nil {
		return nil, err
	}
	w, err := s.store.ApplyWorkflow(ctx, wf.Name, yaml, string(specJSON), actor)
	if err != nil {
		return nil, err
	}
	if wf.On != nil && wf.On.Webhook != nil && w.WebhookToken == "" {
		tok := make([]byte, 16)
		if _, err := rand.Read(tok); err != nil {
			return nil, err
		}
		w.WebhookToken = hex.EncodeToString(tok)
		if err := s.store.SetWebhook(ctx, w.ID, w.WebhookToken, nil); err != nil {
			return nil, err
		}
	}
	s.log.Info("workflow applied", "name", w.Name, "version", w.Version, "actor", actor)
	return s.info(w, wf, true), nil
}

// GetWorkflow returns a workflow with its YAML.
func (s *Service) GetWorkflow(ctx context.Context, name string) (*WorkflowInfo, error) {
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
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

// DeleteWorkflow removes a workflow definition (runs are kept).
func (s *Service) DeleteWorkflow(ctx context.Context, name string) error {
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.store.DeleteWorkflow(ctx, w.ID)
}

// SetWorkflowStatus enables or disables a workflow.
func (s *Service) SetWorkflowStatus(ctx context.Context, name, status string) error {
	if status != store.WorkflowActive && status != store.WorkflowDisabled {
		return badRequest("status must be active or disabled")
	}
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.store.SetWorkflowStatus(ctx, w.ID, status)
}

// ListWorkflowVersions returns version history, newest first.
func (s *Service) ListWorkflowVersions(ctx context.Context, name string) ([]VersionInfo, error) {
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	vs, err := s.store.ListWorkflowVersions(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	out := make([]VersionInfo, 0, len(vs))
	for _, v := range vs {
		out = append(out, VersionInfo{Version: v.Version, Actor: v.Actor, CreatedAt: v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")})
	}
	return out, nil
}

// GetWorkflowVersion returns one version with its YAML.
func (s *Service) GetWorkflowVersion(ctx context.Context, name string, version int) (*VersionInfo, error) {
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
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
	return &VersionInfo{Version: v.Version, Actor: v.Actor, CreatedAt: v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), YAML: v.SpecYAML}, nil
}

func (s *Service) info(w *store.Workflow, wf *workflow.Workflow, full bool) *WorkflowInfo {
	if wf == nil {
		wf = &workflow.Workflow{}
		_ = json.Unmarshal([]byte(w.SpecJSON), wf)
	}
	info := &WorkflowInfo{
		Name: w.Name, Description: wf.Description, Version: w.Version, Status: w.Status, WebhookToken: w.WebhookToken,
		CreatedAt: w.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), UpdatedAt: w.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	for _, st := range wf.Steps {
		info.Steps = append(info.Steps, st.Name)
	}
	if wf.On != nil {
		for _, sc := range wf.On.Schedule {
			info.Schedules = append(info.Schedules, sc.Cron)
		}
	}
	if full {
		info.YAML = w.SpecYAML
		info.Spec = json.RawMessage(w.SpecJSON)
	}
	return info
}

// stepNames extracts step names from stored spec JSON.
func stepNames(specJSON string) ([]string, *workflow.Workflow, error) {
	var wf workflow.Workflow
	if err := json.Unmarshal([]byte(specJSON), &wf); err != nil {
		return nil, nil, err
	}
	names := make([]string, len(wf.Steps))
	for i, st := range wf.Steps {
		names[i] = st.Name
	}
	return names, &wf, nil
}
