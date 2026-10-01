package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/ggrocco/icaro/internal/runner/logs"
	"github.com/ggrocco/icaro/internal/store"
	"github.com/ggrocco/icaro/internal/workflow"
)

// RunInfo is the API view of a run.
type RunInfo struct {
	ID          string          `json:"id"`
	Workflow    string          `json:"workflow"`
	Version     int             `json:"version"`
	Status      string          `json:"status"`
	Trigger     string          `json:"trigger"`
	Input       json.RawMessage `json:"input"`
	RootRunID   string          `json:"root_run_id"`
	ParentRunID string          `json:"parent_run_id,omitempty"`
	Error       string          `json:"error,omitempty"`
	CreatedAt   string          `json:"created_at"`
	StartedAt   *string         `json:"started_at,omitempty"`
	FinishedAt  *string         `json:"finished_at,omitempty"`
	Steps       []StepInfo      `json:"steps,omitempty"`
}

// StepInfo is the API view of a run step.
type StepInfo struct {
	Idx        int             `json:"idx"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Attempt    int             `json:"attempt"`
	ExitCode   *int            `json:"exit_code,omitempty"`
	Outputs    json.RawMessage `json:"outputs"`
	Error      string          `json:"error,omitempty"`
	LogSize    int64           `json:"log_size"`
	StartedAt  *string         `json:"started_at,omitempty"`
	FinishedAt *string         `json:"finished_at,omitempty"`
}

// EnqueueOptions parameterize a new run.
type EnqueueOptions struct {
	Trigger string
	Parent  *store.Run
}

// EnqueueRun validates input against the workflow's declared inputs and
// creates a queued run.
func (s *Service) EnqueueRun(ctx context.Context, name string, input map[string]any, opts EnqueueOptions) (*RunInfo, error) {
	w, err := s.store.GetWorkflow(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if w.Status != store.WorkflowActive {
		return nil, badRequest("workflow %q is %s", name, w.Status)
	}
	names, wf, err := stepNames(w.SpecJSON)
	if err != nil {
		return nil, err
	}
	if input == nil {
		input = map[string]any{}
	}
	if err := applyInputs(wf, input); err != nil {
		return nil, err
	}
	in, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	trigger := opts.Trigger
	if trigger == "" {
		trigger = store.TriggerAPI
	}
	run := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: w.Version, TriggerKind: trigger, InputJSON: string(in)}
	if opts.Parent != nil {
		run.ParentRunID = opts.Parent.ID
		run.RootRunID = opts.Parent.RootRunID
		run.Depth = opts.Parent.Depth + 1
		if run.Depth > 5 {
			return nil, badRequest("run chain depth exceeds 5")
		}
	}
	if err := s.store.CreateRun(ctx, run, names); err != nil {
		return nil, err
	}
	if s.runner != nil {
		s.runner.Wake()
	}
	s.log.Info("run queued", "run", run.ID, "workflow", w.Name, "trigger", trigger)
	return s.GetRun(ctx, run.ID)
}

// applyInputs fills defaults and checks required inputs.
func applyInputs(wf *workflow.Workflow, input map[string]any) error {
	for name, def := range wf.Inputs {
		if _, ok := input[name]; ok {
			continue
		}
		if def.Default != nil {
			input[name] = def.Default
			continue
		}
		if def.Required {
			return badRequest("missing required input %q", name)
		}
	}
	return nil
}

// GetRun returns a run with its steps.
func (s *Service) GetRun(ctx context.Context, id string) (*RunInfo, error) {
	r, err := s.store.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	steps, err := s.store.GetRunSteps(ctx, id)
	if err != nil {
		return nil, err
	}
	info := runInfo(r)
	for _, st := range steps {
		info.Steps = append(info.Steps, StepInfo{
			Idx: st.Idx, Name: st.Name, Status: st.Status, Attempt: st.Attempt, ExitCode: st.ExitCode,
			Outputs: json.RawMessage(orJSON(st.OutputJSON)), Error: st.Error, LogSize: st.LogSize,
			StartedAt: tsp(st.StartedAt), FinishedAt: tsp(st.FinishedAt),
		})
	}
	return info, nil
}

// ListRuns returns run summaries (no steps).
func (s *Service) ListRuns(ctx context.Context, workflowName, status string, limit int) ([]*RunInfo, error) {
	f := store.RunFilter{Status: status, Limit: limit}
	if workflowName != "" {
		w, err := s.store.GetWorkflow(ctx, workflowName)
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		f.WorkflowID = w.ID
	}
	runs, err := s.store.ListRuns(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]*RunInfo, 0, len(runs))
	for _, r := range runs {
		out = append(out, runInfo(r))
	}
	return out, nil
}

// CancelRun requests cancellation.
func (s *Service) CancelRun(ctx context.Context, id string) (*RunInfo, error) {
	r, err := s.store.RequestCancel(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if r.Status == store.RunRunning && s.runner != nil {
		s.runner.Cancel(id)
	}
	return s.GetRun(ctx, id)
}

// StepLogs returns up to tailBytes from the end of a step's log.
func (s *Service) StepLogs(ctx context.Context, id string, idx int, tailBytes int64) ([]byte, error) {
	st, err := s.store.GetRunStep(ctx, id, idx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if st.LogPath == "" {
		return nil, nil
	}
	if tailBytes <= 0 {
		tailBytes = 1 << 20
	}
	b, err := logs.Tail(st.LogPath, tailBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func runInfo(r *store.Run) *RunInfo {
	return &RunInfo{
		ID: r.ID, Workflow: r.WorkflowName, Version: r.WorkflowVersion, Status: r.Status, Trigger: r.TriggerKind,
		Input: json.RawMessage(orJSON(r.InputJSON)), RootRunID: r.RootRunID, ParentRunID: r.ParentRunID, Error: r.Error,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), StartedAt: tsp(r.StartedAt), FinishedAt: tsp(r.FinishedAt),
	}
}

func orJSON(s string) string {
	if s == "" {
		return "{}"
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
