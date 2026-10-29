package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/engine"
	"icaro/internal/workflow/events"
)

// RunInfo is the API view of a run.
type RunInfo struct {
	ID          string          `json:"id"`
	Workflow    string          `json:"workflow"`
	Version     int             `json:"version"`
	Status      string          `json:"status"`
	TriggerKind string          `json:"trigger_kind"`
	Trigger     json.RawMessage `json:"trigger"`
	Concurrency string          `json:"concurrency"`
	SessionID   string          `json:"session_id,omitempty"`
	DeliveryID  string          `json:"delivery_id,omitempty"`
	Actor       string          `json:"actor,omitempty"`
	RootRunID   string          `json:"root_run_id"`
	ParentRunID string          `json:"parent_run_id,omitempty"`
	RerunOf     string          `json:"rerun_of,omitempty"`
	Error       string          `json:"error,omitempty"`
	CreatedAt   string          `json:"created_at"`
	StartedAt   *string         `json:"started_at,omitempty"`
	FinishedAt  *string         `json:"finished_at,omitempty"`
	Steps       []StepInfo      `json:"steps,omitempty"`
}

// StepInfo is the API view of a step of a run.
type StepInfo struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Needs      []string        `json:"needs"`
	Status     string          `json:"status"`
	Attempt    int             `json:"attempt"`
	ExitCode   *int            `json:"exit_code,omitempty"`
	Outputs    json.RawMessage `json:"outputs"`
	Error      string          `json:"error,omitempty"`
	LogSize    int64           `json:"log_size"`
	StartedAt  *string         `json:"started_at,omitempty"`
	FinishedAt *string         `json:"finished_at,omitempty"`
}

// Terminal reports whether a run status is final.
func Terminal(status string) bool {
	switch status {
	case store.RunSucceeded, store.RunFailed, store.RunCancelled:
		return true
	}
	return false
}

// EnqueueRun starts a run (workflow.Enqueuer). It deduplicates on the
// idempotency key and applies the workflow's concurrency policy: forbid
// skips while a run is active, queue waits for the active run, replace
// cancels active runs first (and waits for them to stop), allow runs
// side by side. The policy check and the insert are one transaction.
func (s *Service) EnqueueRun(ctx context.Context, req workflow.EnqueueRequest) (workflow.EnqueueResult, error) {
	w, err := s.lookup(ctx, req.Workflow)
	if err != nil {
		return workflow.EnqueueResult{}, err
	}
	if w.Status != store.WorkflowActive {
		return workflow.EnqueueResult{}, badRequest("workflow %q is %s", w.Name, w.Status)
	}
	if req.IdempotencyKey != "" {
		if prev, err := s.store.GetRunByIdempotencyKey(ctx, req.IdempotencyKey); err == nil {
			return workflow.EnqueueResult{RunID: prev.ID, Deduplicated: true}, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return workflow.EnqueueResult{}, err
		}
	}
	spec, err := decodeSpec(w.SpecJSON)
	if err != nil {
		return workflow.EnqueueResult{}, err
	}
	version := w.Version
	if req.Version != 0 && req.Version != w.Version {
		v, err := s.store.GetWorkflowVersion(ctx, w.ID, req.Version)
		if errors.Is(err, store.ErrNotFound) {
			return workflow.EnqueueResult{}, ErrNotFound
		}
		if err != nil {
			return workflow.EnqueueResult{}, err
		}
		old, issues, err := workflow.Parse([]byte(v.SpecYAML), workflow.Options{})
		if err == nil && len(issues) > 0 {
			err = issues
		}
		if err != nil || old == nil {
			return workflow.EnqueueResult{}, badRequest("version %d of %s no longer parses: %v", req.Version, w.Name, err)
		}
		spec, version = old, req.Version
	}
	policy := spec.ConcurrencyPolicy()
	steps, err := engine.StepInits(spec)
	if err != nil {
		return workflow.EnqueueResult{}, err
	}
	trigger := req.Trigger
	if trigger == nil {
		trigger = map[string]any{}
	}
	tj, err := json.Marshal(trigger)
	if err != nil {
		return workflow.EnqueueResult{}, badRequest("trigger payload: %v", err)
	}
	kind := req.TriggerKind
	if kind == "" {
		kind = workflow.RunTriggerManual
	}
	run := &store.Run{
		WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: version, TriggerKind: kind, TriggerJSON: string(tj),
		IdempotencyKey: req.IdempotencyKey, Concurrency: policy, DeliveryID: req.DeliveryID, Actor: req.Actor,
		RerunOf: req.RerunOf,
	}
	out, err := s.store.EnqueueRun(ctx, run, steps)
	if err != nil {
		if errors.Is(err, store.ErrConflict) && req.IdempotencyKey != "" {
			if prev, err := s.store.GetRunByIdempotencyKey(ctx, req.IdempotencyKey); err == nil {
				return workflow.EnqueueResult{RunID: prev.ID, Deduplicated: true}, nil
			}
		}
		return workflow.EnqueueResult{}, err
	}
	if b := out.Blocking; b != nil {
		reason := fmt.Sprintf("concurrency forbid: run %s is %s", b.ID, b.Status)
		s.log.Info("run skipped", "workflow", w.Name, "reason", reason)
		return workflow.EnqueueResult{Skipped: reason}, nil
	}
	for _, old := range out.Replaced {
		s.log.Info("run replaced", "run", old.ID, "workflow", w.Name, "by", run.ID)
		if old.Status == store.RunRunning {
			if s.runner != nil {
				s.runner.Cancel(old.ID)
			}
		} else {
			s.events.Publish(events.Event{Type: events.TypeRun, RunID: old.ID, Status: store.RunCancelled})
		}
	}
	if s.runner != nil {
		s.runner.Wake()
	}
	s.events.Publish(events.Event{Type: events.TypeRun, RunID: run.ID, Status: store.RunQueued})
	s.log.Info("run queued", "run", run.ID, "workflow", w.Name, "trigger", kind, "policy", policy)
	return workflow.EnqueueResult{RunID: run.ID}, nil
}

// MissingTriggerFieldsError lists required trigger fields a manual run
// did not supply.
type MissingTriggerFieldsError struct {
	Fields []string
}

func (e *MissingTriggerFieldsError) Error() string {
	return "missing trigger field(s): " + strings.Join(e.Fields, ", ")
}

// StartRun is a manual run: every trigger.* field the workflow requires
// must be supplied (the CLI prompts for them).
func (s *Service) StartRun(ctx context.Context, name string, trigger map[string]any, actor string) (*RunInfo, error) {
	w, err := s.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	spec, err := decodeSpec(w.SpecJSON)
	if err != nil {
		return nil, err
	}
	req, _ := workflow.TriggerFields(spec)
	var missing []string
	for _, f := range req {
		if _, ok := trigger[f]; !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, &MissingTriggerFieldsError{Fields: missing}
	}
	res, err := s.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: name, TriggerKind: workflow.RunTriggerManual, Trigger: trigger, Actor: actor})
	if err != nil {
		return nil, err
	}
	if res.Skipped != "" {
		return nil, &Conflict{Msg: res.Skipped}
	}
	return s.GetRun(ctx, res.RunID)
}

// RerunRun starts a run again: the same workflow version, trigger kind
// and payload, linked to the original by rerun_of. A deleted workflow, or
// a version no run references any more, cannot be re-run.
func (s *Service) RerunRun(ctx context.Context, id, actor string) (*RunInfo, error) {
	r, err := s.store.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var trigger map[string]any
	if err := json.Unmarshal([]byte(orJSON(r.TriggerJSON)), &trigger); err != nil {
		return nil, fmt.Errorf("run %s trigger: %w", id, err)
	}
	res, err := s.EnqueueRun(ctx, workflow.EnqueueRequest{
		Workflow: r.WorkflowName, TriggerKind: r.TriggerKind, Trigger: trigger, Actor: actor,
		Version: r.WorkflowVersion, RerunOf: r.ID,
	})
	if err != nil {
		return nil, err
	}
	if res.Skipped != "" {
		return nil, &Conflict{Msg: res.Skipped}
	}
	return s.GetRun(ctx, res.RunID)
}

// GetRun returns a run with its steps in plan order.
func (s *Service) GetRun(ctx context.Context, id string) (*RunInfo, error) {
	r, err := s.store.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	steps, err := s.store.GetStepRuns(ctx, id)
	if err != nil {
		return nil, err
	}
	needs := map[string][]string{}
	if v, err := s.store.GetWorkflowVersion(ctx, r.WorkflowID, r.WorkflowVersion); err == nil {
		if spec, _, err := workflow.Parse([]byte(v.SpecYAML), workflow.Options{}); err == nil && spec != nil {
			for _, st := range spec.Steps {
				needs[st.ID] = st.Needs
			}
		}
	}
	info := runInfo(r)
	for _, st := range steps {
		info.Steps = append(info.Steps, StepInfo{
			ID: st.StepID, Kind: st.Kind, Needs: orEmpty(needs[st.StepID]), Status: st.Status, Attempt: st.Attempt, ExitCode: st.ExitCode,
			Outputs: json.RawMessage(orJSON(st.OutputsJSON)), Error: st.Error, LogSize: st.LogSize,
			StartedAt: tsp(st.StartedAt), FinishedAt: tsp(st.FinishedAt),
		})
	}
	return info, nil
}

// ListRuns returns run summaries (no steps).
func (s *Service) ListRuns(ctx context.Context, workflowName, status string, limit int) ([]*RunInfo, error) {
	f := store.RunFilter{Status: status, Limit: limit}
	if workflowName != "" {
		w, err := s.lookup(ctx, workflowName)
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

// CancelRun requests cancellation; the engine kills running containers.
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
	if r.Status == store.RunCancelled {
		s.events.Publish(events.Event{Type: events.TypeRun, RunID: id, Status: store.RunCancelled})
	}
	return s.GetRun(ctx, id)
}

// LogPage is a slice of a step attempt's log.
type LogPage struct {
	Data    []byte
	Offset  int64 // file offset of Data[0]
	Size    int64 // file size when read
	Attempt int
}

// StepLogs reads a step attempt's log (attempt 0 = latest) from offset,
// or its last tail bytes when tail > 0.
func (s *Service) StepLogs(ctx context.Context, runID, stepID string, attempt int, offset, tail int64) (*LogPage, error) {
	st, err := s.store.GetStepRun(ctx, runID, stepID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if attempt <= 0 {
		attempt = st.Attempt
	}
	page := &LogPage{Attempt: attempt}
	if attempt <= 0 || st.LogPath == "" {
		return page, nil
	}
	path := st.LogPath
	if attempt != st.Attempt {
		path = strings.TrimSuffix(path, fmt.Sprintf(".%d.log", st.Attempt)) + fmt.Sprintf(".%d.log", attempt)
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return page, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	page.Size = fi.Size()
	const maxPage = 4 << 20
	start := offset
	if tail > 0 {
		start = max(page.Size-tail, 0)
	}
	if start < 0 || start > page.Size {
		start = page.Size
	}
	n := min(page.Size-start, maxPage)
	page.Offset = start
	page.Data = make([]byte, n)
	if _, err := f.ReadAt(page.Data, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if tail > 0 && start > 0 {
		// Start at a line boundary.
		if i := strings.IndexByte(string(page.Data), '\n'); i >= 0 {
			page.Data, page.Offset = page.Data[i+1:], start+int64(i+1)
		}
	}
	return page, nil
}

// SubscribeRun follows a run's events.
func (s *Service) SubscribeRun(runID string) (*events.Subscription, error) {
	if s.events == nil {
		return nil, errors.New("run events are not available")
	}
	return s.events.Subscribe(runID), nil
}

func runInfo(r *store.Run) *RunInfo {
	return &RunInfo{
		ID: r.ID, Workflow: r.WorkflowName, Version: r.WorkflowVersion, Status: r.Status, TriggerKind: r.TriggerKind,
		Trigger: json.RawMessage(orJSON(r.TriggerJSON)), Concurrency: r.Concurrency, SessionID: r.SessionID,
		DeliveryID: r.DeliveryID, Actor: r.Actor, RootRunID: r.RootRunID, ParentRunID: r.ParentRunID, RerunOf: r.RerunOf, Error: r.Error,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), StartedAt: tsp(r.StartedAt), FinishedAt: tsp(r.FinishedAt),
	}
}

func orJSON(s string) string {
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}
