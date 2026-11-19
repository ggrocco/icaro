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
	// Depth is how many calls deep the run is (0: a root, ADR 0038), and
	// Outputs its workflow outputs once it succeeded.
	Depth      int             `json:"depth"`
	Outputs    json.RawMessage `json:"outputs"`
	RerunOf    string          `json:"rerun_of,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  string          `json:"created_at"`
	StartedAt  *string         `json:"started_at,omitempty"`
	FinishedAt *string         `json:"finished_at,omitempty"`
	Steps      []StepInfo      `json:"steps,omitempty"`
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
	Approval   *ApprovalInfo   `json:"approval,omitempty"`
	Agent      *AgentStepInfo  `json:"agent,omitempty"`
	Call       *CallStepInfo   `json:"call,omitempty"`
}

// CallStepInfo is the API view of a call step (ADR 0038): the workflow it
// calls and the child run of its latest attempt, with that run's status
// and the called version. Its outputs are the child's workflow outputs.
type CallStepInfo struct {
	Workflow string `json:"workflow"`
	RunID    string `json:"run_id,omitempty"`
	Status   string `json:"status,omitempty"`
	Version  int    `json:"version,omitempty"`
}

// AgentStepInfo is the API view of an agent step (ADR 0037): what ran it,
// what it was asked and what it used. Its final answer is outputs.result.
type AgentStepInfo struct {
	Profile     string         `json:"profile"`
	CLI         string         `json:"cli,omitempty"`
	ImageDigest string         `json:"image_digest,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
	Memory      bool           `json:"memory"`
	// Prompt is the rendered, secret-redacted prompt of the latest attempt.
	Prompt string         `json:"prompt,omitempty"`
	Usage  AgentUsageInfo `json:"usage"`
}

// AgentUsageInfo is an agent step's token usage over its attempts.
// CostUSD is the CLI's own figure, an estimate, set only for inject
// (API-key) profiles.
type AgentUsageInfo struct {
	Input      int64    `json:"input"`
	Output     int64    `json:"output"`
	CacheRead  int64    `json:"cache_read,omitempty"`
	CacheWrite int64    `json:"cache_write,omitempty"`
	CostUSD    *float64 `json:"cost_usd_estimate,omitempty"`
}

// maxPromptShown caps the prompt returned with a run.
const maxPromptShown = 64 << 10

// agentInfo describes an agent step from its row and its spec.
func (s *Service) agentInfo(st *store.StepRun, spec *workflow.AgentSpec) *AgentStepInfo {
	a := &AgentStepInfo{Profile: st.Profile, ImageDigest: st.ImageDigest, Memory: true,
		Usage: AgentUsageInfo{Input: st.Usage.Input, Output: st.Usage.Output, CacheRead: st.Usage.CacheRead, CacheWrite: st.Usage.CacheWrite}}
	if st.Usage.CostMicro > 0 {
		c := float64(st.Usage.CostMicro) / 1e6
		a.Usage.CostUSD = &c
	}
	if spec != nil {
		a.Params, a.Memory = spec.Params, spec.MemoryOn()
		if a.Profile == "" {
			if p, ok := s.agents.AgentStepProfile(spec); ok {
				a.Profile = p.Name
			} else {
				a.Profile = spec.Profile
			}
		}
	}
	for _, p := range s.agents.Profiles {
		if p.Name == a.Profile {
			a.CLI = p.Agent
		}
	}
	if st.LogPath != "" {
		if f, err := os.Open(strings.TrimSuffix(st.LogPath, ".log") + ".prompt.md"); err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, maxPromptShown))
			_ = f.Close()
			a.Prompt = string(b)
		}
	}
	return a
}

// ApprovalInfo is the API view of an approval step's request and decision.
// Decision is empty while it waits; Deadline is when an undecided
// approval expires (none: it waits until decided or cancelled).
type ApprovalInfo struct {
	Message   string  `json:"message"`
	Decision  string  `json:"decision,omitempty"`
	Actor     string  `json:"actor,omitempty"`
	Comment   string  `json:"comment,omitempty"`
	DecidedAt *string `json:"decided_at,omitempty"`
	Deadline  *string `json:"deadline,omitempty"`
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
			return workflow.EnqueueResult{RunID: prev.ID, Deduplicated: true, Version: prev.WorkflowVersion}, nil
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
	var out store.PolicyOutcome
	if req.ParentRunID != "" {
		booking, err := s.callChild(ctx, req, spec, run)
		if err != nil {
			return workflow.EnqueueResult{}, err
		}
		out, err = s.store.EnqueueCall(ctx, run, steps, booking)
		if errors.Is(err, store.ErrRootBudget) {
			return workflow.EnqueueResult{}, &Conflict{Msg: fmt.Sprintf("workflows.max_runs_per_root (%d) reached: the tree of run %s has run %d times; nothing was started", s.maxRunsPerRoot, run.RootRunID, s.maxRunsPerRoot)}
		}
	} else {
		out, err = s.store.EnqueueRun(ctx, run, steps)
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) && req.IdempotencyKey != "" {
			if prev, err := s.store.GetRunByIdempotencyKey(ctx, req.IdempotencyKey); err == nil {
				return workflow.EnqueueResult{RunID: prev.ID, Deduplicated: true, Version: prev.WorkflowVersion}, nil
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
		s.stopActive(old)
	}
	if s.runner != nil {
		s.runner.Wake()
	}
	s.events.Publish(events.Event{Type: events.TypeRun, RunID: run.ID, Status: store.RunQueued})
	s.log.Info("run queued", "run", run.ID, "workflow", w.Name, "trigger", kind, "policy", run.Concurrency)
	return workflow.EnqueueResult{RunID: run.ID, Version: version}, nil
}

// callChild makes run the child of a call step (ADR 0038): it joins the
// parent's tree one level down, the depth cap applies here and the
// per-root budget in the store's transaction, and replace acts as queue
// so a call never cancels a run another parent may be waiting on.
func (s *Service) callChild(ctx context.Context, req workflow.EnqueueRequest, spec *workflow.Spec, run *store.Run) (store.CallBooking, error) {
	parent, err := s.store.GetRun(ctx, req.ParentRunID)
	if errors.Is(err, store.ErrNotFound) {
		return store.CallBooking{}, badRequest("parent run %s does not exist", req.ParentRunID)
	}
	if err != nil {
		return store.CallBooking{}, err
	}
	if !spec.Callable() {
		return store.CallBooking{}, badRequest("workflow %s has no call trigger (version %d), so it cannot be called", run.WorkflowName, run.WorkflowVersion)
	}
	run.RootRunID, run.ParentRunID, run.Depth = parent.RootRunID, parent.ID, parent.Depth+1
	if run.Depth > s.maxCallDepth {
		return store.CallBooking{}, &Conflict{Msg: fmt.Sprintf("workflows.max_call_depth (%d) reached: run %s is already %d calls deep; nothing was started", s.maxCallDepth, parent.ID, parent.Depth)}
	}
	if run.Concurrency == workflow.ConcurrencyReplace {
		run.Concurrency = workflow.ConcurrencyQueue
	}
	return store.CallBooking{ParentRunID: parent.ID, StepID: req.ParentStep, MaxRunsPerRoot: s.maxRunsPerRoot}, nil
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
	timeouts := map[string]string{}
	agents := map[string]*workflow.AgentSpec{}
	calls := map[string]*workflow.CallSpec{}
	if v, err := s.store.GetWorkflowVersion(ctx, r.WorkflowID, r.WorkflowVersion); err == nil {
		if spec, _, err := workflow.Parse([]byte(v.SpecYAML), workflow.Options{}); err == nil && spec != nil {
			for _, st := range spec.Steps {
				needs[st.ID], timeouts[st.ID], agents[st.ID], calls[st.ID] = st.Needs, st.Timeout, st.Agent, st.Call
			}
		}
	}
	approvals, err := s.store.ListApprovals(ctx, id)
	if err != nil {
		return nil, err
	}
	byStep := map[string]*store.Approval{}
	for _, a := range approvals {
		byStep[a.StepID] = a
	}
	info := runInfo(r)
	for _, st := range steps {
		si := StepInfo{
			ID: st.StepID, Kind: st.Kind, Needs: orEmpty(needs[st.StepID]), Status: st.Status, Attempt: st.Attempt, ExitCode: st.ExitCode,
			Outputs: json.RawMessage(orJSON(st.OutputsJSON)), Error: st.Error, LogSize: st.LogSize,
			StartedAt: tsp(st.StartedAt), FinishedAt: tsp(st.FinishedAt),
		}
		if a := byStep[st.StepID]; a != nil {
			si.Approval = &ApprovalInfo{Message: a.Message, Decision: a.Decision, Actor: a.Actor, Comment: a.Comment, DecidedAt: tsp(a.DecidedAt)}
			if d, err := time.ParseDuration(timeouts[st.StepID]); err == nil && st.StartedAt != nil && a.Decision == "" {
				si.Approval.Deadline = tsp(new(st.StartedAt.Add(d)))
			}
		}
		if st.Kind == workflow.KindAgent {
			si.Agent = s.agentInfo(st, agents[st.StepID])
		}
		if st.Kind == workflow.KindCall {
			si.Call = &CallStepInfo{RunID: st.ChildRunID, Version: st.ChildVersion}
			if c := calls[st.StepID]; c != nil {
				si.Call.Workflow = c.Workflow
			}
			if st.ChildRunID != "" {
				if child, err := s.store.GetRun(ctx, st.ChildRunID); err == nil {
					si.Call.Status, si.Call.Workflow = child.Status, child.WorkflowName
				}
			}
		}
		info.Steps = append(info.Steps, si)
	}
	return info, nil
}

// DecideApproval approves or rejects an approval step that is waiting.
// A rejection fails the step, and the steps that need it are skipped.
func (s *Service) DecideApproval(ctx context.Context, runID, stepID string, approve bool, actor, comment string) (*RunInfo, error) {
	if len(comment) > maxApprovalComment {
		return nil, badRequest("comment is longer than %d bytes", maxApprovalComment)
	}
	st, err := s.store.GetStepRun(ctx, runID, stepID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if st.Kind != workflow.KindApproval {
		return nil, badRequest("step %s is not an approval step", stepID)
	}
	if st.Status != store.StepWaiting {
		return nil, &Conflict{Msg: fmt.Sprintf("step %s is %s, not waiting for approval", stepID, st.Status)}
	}
	decision := store.ApprovalRejected
	if approve {
		decision = store.ApprovalApproved
	}
	if _, err := s.store.DecideApproval(ctx, runID, stepID, decision, actor, comment); errors.Is(err, store.ErrNotFound) {
		return nil, &Conflict{Msg: fmt.Sprintf("step %s is not waiting for approval yet", stepID)}
	} else if errors.Is(err, store.ErrConflict) {
		return nil, &Conflict{Msg: fmt.Sprintf("step %s was already decided", stepID)}
	} else if err != nil {
		return nil, err
	}
	s.log.Info("approval decided", "run", runID, "step", stepID, "decision", decision, "actor", actor)
	if s.runner != nil {
		s.runner.Wake()
	}
	return s.GetRun(ctx, runID)
}

// maxApprovalComment bounds the comment a decision carries; it becomes
// the step's comment output.
const maxApprovalComment = 2000

// stopActive acts on a cancel request the store just recorded: a running
// run is stopped by its engine, a parked one is woken to cancel its
// waiting steps, and a queued one was cancelled outright.
func (s *Service) stopActive(r *store.Run) {
	switch r.Status {
	case store.RunRunning:
		if s.runner != nil {
			s.runner.Cancel(r.ID)
		}
	case store.RunWaiting:
		if s.runner != nil {
			s.runner.Wake()
		}
	default:
		s.events.Publish(events.Event{Type: events.TypeRun, RunID: r.ID, Status: store.RunCancelled})
	}
}

// ListRuns returns run summaries (no steps).
func (s *Service) ListRuns(ctx context.Context, workflowName, status string, limit int) ([]*RunInfo, error) {
	return s.ListRunsFiltered(ctx, RunQuery{Workflow: workflowName, Status: status, Limit: limit})
}

// RunQuery filters ListRunsFiltered. Root lists one run tree
// (ADR 0038): the root and every run that shares it.
type RunQuery struct {
	Workflow, Status, Root string
	Limit                  int
}

// ListRunsFiltered returns run summaries (no steps), newest first.
func (s *Service) ListRunsFiltered(ctx context.Context, q RunQuery) ([]*RunInfo, error) {
	workflowName := q.Workflow
	f := store.RunFilter{Status: q.Status, RootRunID: q.Root, Limit: q.Limit}
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
	switch r.Status {
	case store.RunRunning, store.RunWaiting:
		s.stopActive(r)
	case store.RunCancelled:
		s.events.Publish(events.Event{Type: events.TypeRun, RunID: id, Status: store.RunCancelled})
	}
	s.stopDescendants(ctx, id)
	return s.GetRun(ctx, id)
}

// stopDescendants acts on the cancel requests the store made down a run's
// tree (ADR 0038), so each child stops now rather than on its heartbeat.
func (s *Service) stopDescendants(ctx context.Context, id string) {
	kids, err := s.store.ActiveDescendants(ctx, id)
	if err != nil {
		s.log.Warn("list child runs to stop", "run", id, "err", err)
		return
	}
	for _, k := range kids {
		s.stopActive(k)
	}
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
		Depth: r.Depth, Outputs: json.RawMessage(orJSON(r.OutputsJSON)),
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), StartedAt: tsp(r.StartedAt), FinishedAt: tsp(r.FinishedAt),
	}
}

func orJSON(s string) string {
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}
