package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"icaro/internal/container"
	"icaro/internal/expr"
	"icaro/internal/logs"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

// notRunPrefix marks a step skipped because a dependency did not succeed
// (as opposed to skipped by its own `when`), so the reason survives a
// restart and propagates to its dependents (ADR 0013).
const notRunPrefix = "not run: "

// runExec is one run's scheduler state, owned by its goroutine.
type runExec struct {
	e       *Engine
	run     *store.Run
	spec    *workflow.Spec
	plan    *workflow.Plan
	rows    map[string]*store.StepRun
	trigger map[string]any
	env     map[string]string // rendered workflow env
	secrets map[string]any    // referenced secrets, for interpolation
	redact  *logs.Redactor
	ws      session.RunWorkspace
	attach  container.Attachment
	resumed bool
}

// stepResult is what a step goroutine reports back to the scheduler.
type stepResult struct {
	id       string
	status   string
	outputs  map[string]any
	err      string
	shutdown bool // daemon stopping: state left for re-attach
}

func (e *Engine) executeRun(ctx context.Context, cancel context.CancelCauseFunc, run *store.Run, resumed bool) {
	log := e.log.With("run", run.ID, "workflow", run.WorkflowName)
	hbDone := make(chan struct{})
	go e.heartbeat(ctx, cancel, run.ID, hbDone)
	defer close(hbDone)

	e.publish(events.Event{Type: events.TypeRun, RunID: run.ID, Status: store.RunRunning})
	status, errMsg, keep := e.runToEnd(ctx, run, resumed)
	if status == "" {
		log.Info("leaving run for re-attach after restart")
		return
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer fcancel()
	// Finish the session (workspace cleanup) before the run turns terminal,
	// so a terminal run never has a live session or workspace behind it. A
	// crash in between leaves a running run with finished steps, which the
	// next engine resumes and closes.
	e.settleLeftovers(fctx, run.ID, status, errMsg)
	cur, err := e.d.Store.GetRun(fctx, run.ID)
	if err == nil && cur.SessionID != "" {
		if err := e.d.Sessions.FinishRun(fctx, cur.SessionID, keep); err != nil {
			log.Warn("finish run session", "err", err)
		}
	}
	if err := e.d.Store.FinishRun(fctx, run.ID, status, errMsg); err != nil {
		log.Error("finish run", "err", err)
	}
	log.Info("run finished", "status", status, "error", errMsg)
	e.publish(events.Event{Type: events.TypeRun, RunID: run.ID, Status: status})
}

// settleLeftovers gives every step still pending or running a terminal
// state with a reason (ADR 0013). After a normal schedule there are none;
// a run that fails or is cancelled before its DAG starts (bad stored
// version, session, egress, secrets, workflow env) leaves them all. A
// step left running by a previous daemon has its container killed.
func (e *Engine) settleLeftovers(ctx context.Context, runID, status, errMsg string) {
	rows, err := e.d.Store.GetStepRuns(ctx, runID)
	if err != nil {
		e.log.Error("load steps to settle", "run", runID, "err", err)
		return
	}
	for _, row := range rows {
		if terminal(row.Status) {
			continue
		}
		if row.Status == store.StepRunning && row.ContainerID != "" {
			_ = e.d.Docker.Kill(ctx, row.ContainerID)
			_ = e.d.Docker.RemoveContainer(ctx, row.ContainerID)
		}
		switch {
		case status == store.RunCancelled:
			row.Status, row.Error = store.StepCancelled, "run cancelled"
		case row.Status == store.StepRunning:
			row.Status, row.Error = store.StepFailed, errMsg
		default:
			row.Status, row.Error = store.StepSkipped, notRunPrefix+errMsg
		}
		now := time.Now()
		row.FinishedAt = &now
		if row.StartedAt == nil {
			row.StartedAt = &now
		}
		if err := e.d.Store.UpdateStepRun(ctx, row); err != nil {
			e.log.Error("persist step", "run", runID, "step", row.StepID, "err", err)
		}
		e.publish(events.Event{Type: events.TypeStep, RunID: runID, Step: row.StepID, Status: row.Status})
	}
}

// heartbeat refreshes liveness and turns a persisted cancel request (made
// through the API by another client) into a cancellation.
func (e *Engine) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, runID string, done <-chan struct{}) {
	t := time.NewTicker(e.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			_ = e.d.Store.Heartbeat(ctx, runID)
			if cur, err := e.d.Store.GetRun(ctx, runID); err == nil && cur.CancelRequestedAt != nil {
				cancel(errCancelled)
			}
		}
	}
}

// runToEnd drives the DAG. It returns "" as status when the daemon is
// shutting down and the run must stay running for re-attach.
func (e *Engine) runToEnd(ctx context.Context, run *store.Run, resumed bool) (status, errMsg string, keep bool) {
	fail := func(format string, args ...any) (string, string, bool) {
		if cancelled(ctx) {
			// A cancel while the run is starting surfaces as whatever
			// call it interrupted; it is still a cancel.
			return store.RunCancelled, "cancelled", false
		}
		return store.RunFailed, fmt.Sprintf(format, args...), e.cfg.KeepWorkspaceOnFailure
	}
	ver, err := e.d.Store.GetWorkflowVersion(ctx, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		if ctx.Err() != nil && !cancelled(ctx) {
			return "", "", false
		}
		return fail("load workflow version: %v", err)
	}
	spec, issues, err := workflow.Parse([]byte(ver.SpecYAML), workflow.Options{})
	if err != nil {
		return fail("parse workflow: %v", err)
	}
	if len(issues) > 0 {
		return fail("workflow is invalid: %s", issues.Error())
	}
	plan, err := workflow.NewPlan(spec)
	if err != nil {
		return fail("plan: %v", err)
	}
	rx := &runExec{e: e, run: run, spec: spec, plan: plan, rows: map[string]*store.StepRun{}, resumed: resumed}
	if err := json.Unmarshal([]byte(run.TriggerJSON), &rx.trigger); err != nil || rx.trigger == nil {
		rx.trigger = map[string]any{}
	}
	rows, err := e.d.Store.GetStepRuns(ctx, run.ID)
	if err != nil {
		return fail("load steps: %v", err)
	}
	for _, r := range rows {
		rx.rows[r.StepID] = r
	}
	for _, id := range plan.Order {
		if rx.rows[id] == nil {
			return fail("run has no state for step %s", id)
		}
	}
	if err := rx.prepare(ctx); err != nil {
		if ctx.Err() != nil && !cancelled(ctx) {
			return "", "", false
		}
		return fail("%v", err)
	}
	return rx.schedule(ctx)
}

func cancelled(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errCancelled) }

// prepare starts the run session, confines its network and resolves the
// workflow env and secrets.
func (rx *runExec) prepare(ctx context.Context) error {
	e := rx.e
	ws, err := e.d.Sessions.StartRun(ctx, rx.run.ID, e.cfg.Profile)
	if err != nil {
		return fmt.Errorf("start run session: %w", err)
	}
	rx.ws = ws
	if ws.SessionID != "" && ws.SessionID != rx.run.SessionID {
		if err := e.d.Store.SetRunSession(ctx, rx.run.ID, ws.SessionID); err != nil {
			return err
		}
		rx.run.SessionID = ws.SessionID
	}
	if e.d.Egress != nil {
		att, err := e.d.Egress.Ensure(ctx, e.cfg.Profile, e.cfg.Allow)
		if err != nil {
			return fmt.Errorf("egress for profile %s: %w", e.cfg.Profile, err)
		}
		rx.attach = att
	}

	// Every known secret value is redacted from logs; only the referenced
	// ones are available to interpolation.
	rx.secrets = map[string]any{}
	var values []string
	if e.d.Secrets != nil {
		names, err := e.d.Secrets.Names(ctx)
		if err != nil {
			return fmt.Errorf("list secrets: %w", err)
		}
		var integrations []string
		if e.d.Catalog != nil {
			integrations = e.d.Catalog.Integrations()
		}
		referenced := map[string]bool{}
		for _, n := range workflow.SecretRefs(rx.spec) {
			referenced[n] = true
			// Integration credentials reach only uses: actions, which send
			// them to their bound hosts (03-security); validation refuses
			// them too, but a binding can be added after the workflow.
			hosts, err := e.d.Secrets.Hosts(ctx, n)
			if err != nil {
				return fmt.Errorf("secret %s: %w", n, err)
			}
			if workflow.IntegrationCredential(n, len(hosts) > 0, integrations) {
				return fmt.Errorf("secret %s is an integration credential and cannot be interpolated: %s", n, workflow.IntegrationCredentialHint(n))
			}
		}
		for _, n := range names {
			v, err := e.d.Secrets.Resolve(ctx, n)
			if err != nil {
				return fmt.Errorf("resolve secret %s: %w", n, err)
			}
			values = append(values, v)
			if referenced[n] {
				rx.secrets[n] = v
			}
		}
	}
	rx.redact = logs.NewRedactor(values)

	base := rx.baseEnv(1)
	rx.env = map[string]string{}
	for k, v := range rx.spec.Env {
		out, err := expr.RenderString(v, base)
		if err != nil {
			return fmt.Errorf("env.%s: %w", k, err)
		}
		rx.env[k] = out
	}
	return nil
}

// baseEnv is the interpolation env without step results.
func (rx *runExec) baseEnv(attempt int) expr.Env {
	envAny := map[string]any{}
	for k, v := range rx.env {
		envAny[k] = v
	}
	return expr.Env{
		"trigger":  rx.trigger,
		"secrets":  rx.secrets,
		"env":      envAny,
		"run":      map[string]any{"id": rx.run.ID, "attempt": float64(attempt), "trigger": rx.run.TriggerKind, "workflow": rx.run.WorkflowName},
		"workflow": map[string]any{"name": rx.run.WorkflowName, "version": float64(rx.run.WorkflowVersion)},
	}
}

// stepsEnv exposes terminal steps' status and outputs.
func (rx *runExec) stepsEnv() map[string]any {
	out := map[string]any{}
	for id, r := range rx.rows {
		if !terminal(r.Status) {
			continue
		}
		outputs := map[string]any{}
		_ = json.Unmarshal([]byte(r.OutputsJSON), &outputs)
		out[id] = map[string]any{"status": r.Status, "outputs": outputs}
	}
	return out
}

func terminal(status string) bool {
	switch status {
	case store.StepSucceeded, store.StepFailed, store.StepSkipped, store.StepCancelled:
		return true
	}
	return false
}

// satisfied reports whether a dependency lets its dependents run:
// succeeded, or skipped by its own `when`.
func satisfied(r *store.StepRun) bool {
	return r.Status == store.StepSucceeded || (r.Status == store.StepSkipped && !strings.HasPrefix(r.Error, notRunPrefix))
}

// schedule runs ready steps until the DAG settles.
func (rx *runExec) schedule(ctx context.Context) (string, string, bool) {
	e := rx.e
	results := make(chan stepResult)
	running := 0
	shutdown := false

	launch := func(id string, resume bool) {
		running++
		row := *rx.rows[id]
		st := rx.spec.Step(id)
		env := rx.baseEnv(1)
		env["steps"] = rx.stepsEnv()
		// The step goroutine owns the persisted row from here; the
		// scheduler only tracks that it is in flight.
		rx.rows[id].Status = store.StepRunning
		go func() { results <- rx.runStep(ctx, st, &row, env, resume) }()
	}

	// Re-attach steps that were running when the daemon stopped.
	for _, id := range rx.plan.Order {
		if rx.rows[id].Status == store.StepRunning {
			launch(id, true)
		}
	}

	for {
		if ctx.Err() == nil {
			for _, id := range rx.plan.Order {
				if e.cfg.RunParallelism > 0 && running >= e.cfg.RunParallelism {
					break
				}
				row := rx.rows[id]
				if row.Status != store.StepPending {
					continue
				}
				ready, blockedBy := rx.readiness(id)
				switch {
				case blockedBy != "":
					rx.settle(ctx, row, store.StepSkipped, notRunPrefix+blockedBy)
				case ready:
					if rx.gate(ctx, id) {
						launch(id, false)
					}
				}
			}
		}
		if running == 0 {
			break
		}
		// Step goroutines return promptly once ctx ends, so this never
		// outlives a cancel or shutdown for long.
		res := <-results
		running--
		if res.shutdown {
			shutdown = true
			continue
		}
		row := rx.rows[res.id]
		row.Status, row.Error = res.status, res.err
		if res.outputs != nil {
			b, _ := json.Marshal(res.outputs)
			row.OutputsJSON = string(b)
		}
	}

	if ctx.Err() != nil && !cancelled(ctx) {
		if shutdown || rx.anyPending() {
			return "", "", false // daemon stopping: re-attach on restart
		}
	} else if cancelled(ctx) {
		fctx := context.WithoutCancel(ctx)
		for _, id := range rx.plan.Order {
			if row := rx.rows[id]; row.Status == store.StepPending {
				rx.settle(fctx, row, store.StepCancelled, "run cancelled")
			}
		}
		return store.RunCancelled, "cancelled", false
	}

	for _, id := range rx.plan.Order {
		if row := rx.rows[id]; row.Status == store.StepFailed {
			return store.RunFailed, fmt.Sprintf("step %s failed: %s", id, row.Error), rx.e.cfg.KeepWorkspaceOnFailure
		}
	}
	return store.RunSucceeded, "", false
}

func (rx *runExec) anyPending() bool {
	for _, r := range rx.rows {
		if !terminal(r.Status) {
			return true
		}
	}
	return false
}

// readiness reports whether all needs are terminal and satisfied, or the
// first need that blocks the step.
func (rx *runExec) readiness(id string) (ready bool, blockedBy string) {
	for _, n := range rx.plan.Needs[id] {
		dep := rx.rows[n]
		if !terminal(dep.Status) {
			return false, ""
		}
		if !satisfied(dep) {
			return false, fmt.Sprintf("needs %s %s", n, dep.Status)
		}
	}
	return true, ""
}

// gate evaluates `when`; false or an error settles the step here.
func (rx *runExec) gate(ctx context.Context, id string) bool {
	st := rx.spec.Step(id)
	if strings.TrimSpace(st.When) == "" {
		return true
	}
	row := rx.rows[id]
	cond, err := expr.ParseCondition(st.When)
	if err != nil {
		rx.settle(ctx, row, store.StepFailed, "when: "+err.Error())
		return false
	}
	env := rx.baseEnv(1)
	env["steps"] = rx.stepsEnv()
	ok, err := cond.EvalBool(env)
	if err != nil {
		rx.settle(ctx, row, store.StepFailed, "when: "+err.Error())
		return false
	}
	if !ok {
		rx.settle(ctx, row, store.StepSkipped, "")
	}
	return ok
}

// settle persists a terminal state decided by the scheduler (skips,
// gating failures, cancellation of never-started steps).
func (rx *runExec) settle(ctx context.Context, row *store.StepRun, status, msg string) {
	now := time.Now()
	row.Status, row.Error, row.FinishedAt = status, msg, &now
	if row.StartedAt == nil {
		row.StartedAt = &now
	}
	if err := rx.e.d.Store.UpdateStepRun(ctx, row); err != nil {
		rx.e.log.Error("persist step", "run", rx.run.ID, "step", row.StepID, "err", err)
	}
	rx.e.publish(events.Event{Type: events.TypeStep, RunID: rx.run.ID, Step: row.StepID, Status: status})
}
