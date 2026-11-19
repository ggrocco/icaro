package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"icaro/internal/expr"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// Call steps (ADR 0038) run another workflow as a child run. The step is
// a pause step: the scheduler starts it by enqueueing the child, looks at
// the child on every pass, and parks the run while it waits, so a chain
// of calls never holds the slots its children need. The child's final
// state wakes the parent in the transaction that records it.

// startCall starts attempt n of a call step: it renders the input and
// enqueues the child, which the store books on the step in the same
// transaction. The idempotency key makes a restart between the enqueue
// and the step's own bookkeeping find the same child, never a second one.
func (rx *runExec) startCall(ctx context.Context, st *workflow.StepSpec, attempt int) {
	row := rx.rows[st.ID]
	now := time.Now()
	row.Status, row.Attempt, row.StartedAt, row.FinishedAt = store.StepWaiting, attempt, &now, nil
	row.LogPath, row.ChildRunID, row.ChildVersion = rx.e.logPath(rx.run.ID, st.ID, attempt), "", 0
	fail := func(msg string) {
		rx.callLog(ctx, row, "error: "+msg)
		rx.finishStep(ctx, row, store.StepFailed, nil, msg)
	}
	if rx.e.d.Calls == nil {
		fail("no workflow service is configured; cannot call " + st.Call.Workflow)
		return
	}
	env := rx.baseEnv(attempt)
	env["steps"] = rx.stepsEnv()
	delete(env, "secrets") // the input is stored with the child run
	in, err := expr.EvalValue(map[string]any(st.Call.Input), env)
	if err != nil {
		fail("input: " + err.Error())
		return
	}
	input, _ := redactValue(rx.redact, in).(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	version, err := rx.calledVersion(ctx, st.ID)
	if err != nil {
		fail(err.Error())
		return
	}
	res, err := rx.e.d.Calls.EnqueueRun(ctx, workflow.EnqueueRequest{
		Workflow: st.Call.Workflow, TriggerKind: workflow.RunTriggerCall, Trigger: input,
		IdempotencyKey: fmt.Sprintf("call:%s:%s:%d", rx.run.ID, st.ID, attempt), Actor: "run:" + rx.run.ID,
		Version: version, ParentRunID: rx.run.ID, ParentStep: st.ID,
	})
	switch {
	case err != nil:
		fail(fmt.Sprintf("call %s: %v", st.Call.Workflow, err))
		return
	case res.Skipped != "":
		fail(fmt.Sprintf("call %s: %s", st.Call.Workflow, res.Skipped))
		return
	}
	row.ChildRunID, row.ChildVersion = res.RunID, res.Version
	rx.callLog(ctx, row, fmt.Sprintf("call %s version %d (attempt %d): child run %s", st.Call.Workflow, res.Version, attempt, res.RunID))
	rx.persist(ctx, row)
}

// calledVersion is the callee version a re-run repeats: the one the
// original run's step recorded (0: the current version).
func (rx *runExec) calledVersion(ctx context.Context, stepID string) (int, error) {
	if rx.run.RerunOf == "" {
		return 0, nil
	}
	orig, err := rx.e.d.Store.GetStepRun(ctx, rx.run.RerunOf, stepID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load the re-run's original step: %w", err)
	}
	return orig.ChildVersion, nil
}

// checkCall settles a call step whose child reached a final state, and
// enforces its timeout. Otherwise it returns the deadline, if any.
func (rx *runExec) checkCall(ctx context.Context, st *workflow.StepSpec) (settled bool, due *time.Time) {
	row := rx.rows[st.ID]
	if row.ChildRunID == "" {
		// The attempt never reached its enqueue (a restart in between):
		// start it again under the same key.
		rx.startCall(ctx, st, max(row.Attempt, 1))
		return true, nil
	}
	child, err := rx.e.d.Store.GetRun(ctx, row.ChildRunID)
	if err != nil {
		rx.finishStep(ctx, row, store.StepFailed, nil, "load child run: "+err.Error())
		return true, nil
	}
	switch child.Status {
	case store.RunSucceeded:
		outputs := map[string]any{}
		_ = json.Unmarshal([]byte(child.OutputsJSON), &outputs)
		rx.callLog(ctx, row, fmt.Sprintf("child run %s succeeded", child.ID))
		rx.finishStep(ctx, row, store.StepSucceeded, outputs, "")
		return true, nil
	case store.RunFailed:
		return rx.callAttemptFailed(ctx, st, fmt.Sprintf("child run %s failed: %s", child.ID, child.Error)), nil
	case store.RunCancelled:
		return rx.callAttemptFailed(ctx, st, fmt.Sprintf("child run %s cancelled", child.ID)), nil
	}
	if st.Timeout == "" || row.StartedAt == nil {
		return false, nil
	}
	d, _ := time.ParseDuration(st.Timeout) // validated
	deadline := row.StartedAt.Add(d)
	if time.Now().Before(deadline) {
		return false, &deadline
	}
	rx.e.stopRun(context.WithoutCancel(ctx), child.ID)
	return rx.callAttemptFailed(ctx, st, fmt.Sprintf("timed out after %s; child run %s cancelled", st.Timeout, child.ID)), nil
}

// callAttemptFailed starts the next attempt, with a new child, while the
// step has retries left; otherwise it fails the step.
func (rx *runExec) callAttemptFailed(ctx context.Context, st *workflow.StepSpec, msg string) bool {
	row := rx.rows[st.ID]
	rx.callLog(ctx, row, "error: "+msg)
	if row.Attempt < st.Retry+1 {
		rx.e.log.Warn("call attempt failed, retrying", "run", rx.run.ID, "step", st.ID, "attempt", row.Attempt, "err", msg)
		prev := row.Attempt
		rx.startCall(ctx, st, prev+1)
		if row.Status == store.StepWaiting {
			row.Error = fmt.Sprintf("attempt %d failed: %s; retrying", prev, msg)
			rx.persist(ctx, row)
		}
		return true
	}
	rx.finishStep(ctx, row, store.StepFailed, nil, msg)
	return true
}

// callLog appends lines to a call step attempt's log: the enqueue, the
// child's id and version, and its outcome. The child keeps its own logs.
func (rx *runExec) callLog(ctx context.Context, row *store.StepRun, lines ...string) {
	lc, err := rx.e.openLog(ctx, rx, row, row.Attempt, true)
	if err != nil {
		rx.e.log.Warn("open call step log", "run", rx.run.ID, "step", row.StepID, "err", err)
		return
	}
	// Append after what the log already has, instead of replaying it.
	lc.lines, lc.skip = lc.skip, 0
	for _, l := range lines {
		lc.line(l)
	}
	lc.close()
	row.LogSize = lc.offset
}

// waitForChildren keeps a cancelled run from being recorded cancelled
// while it still has active child runs (ADR 0038: cancelling goes down
// the tree). It asks them to stop and parks the run, without a slot,
// until the next poll or a child's end wakes it.
func (e *Engine) waitForChildren(ctx context.Context, runID string) bool {
	kids, err := e.d.Store.ActiveChildren(ctx, runID)
	if err != nil || len(kids) == 0 {
		return false
	}
	for _, k := range kids {
		e.stopRun(ctx, k.ID)
	}
	next := time.Now().Add(e.cfg.PollInterval)
	if err := e.d.Store.ParkRun(ctx, runID, &next); err != nil {
		e.log.Warn("park cancelled run", "run", runID, "err", err)
		return false
	}
	return true
}

// stopRun asks a run, and its tree below it, to stop: the store cancels
// queued runs and flags the others, which this engine then stops.
func (e *Engine) stopRun(ctx context.Context, id string) {
	r, err := e.d.Store.RequestCancel(ctx, id)
	if err != nil {
		e.log.Warn("cancel child run", "run", id, "err", err)
		return
	}
	runs := []*store.Run{r}
	if kids, err := e.d.Store.ActiveDescendants(ctx, id); err == nil {
		runs = append(runs, kids...)
	}
	for _, k := range runs {
		switch k.Status {
		case store.RunRunning:
			e.Cancel(k.ID)
		case store.RunWaiting:
			e.Wake()
		}
	}
}

func terminalRun(status string) bool {
	return status == store.RunSucceeded || status == store.RunFailed || status == store.RunCancelled
}

// workflowOutputs evaluates the workflow's outputs once the run succeeded
// (ADR 0038): over its steps' outputs, without secrets, redacted and
// capped like step outputs.
func (rx *runExec) workflowOutputs() (string, error) {
	if len(rx.spec.Outputs) == 0 {
		return "", nil
	}
	env := rx.baseEnv(1)
	env["steps"] = rx.stepsEnv()
	delete(env, "secrets")
	out := map[string]any{}
	for _, k := range sortStrings(keysOf(rx.spec.Outputs)) {
		v, err := expr.EvalValue(rx.spec.Outputs[k], env)
		if err != nil {
			return "", fmt.Errorf("outputs.%s: %w", k, err)
		}
		out[k] = redactValue(rx.redact, v)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("outputs: %w", err)
	}
	if int64(len(b)) > rx.e.cfg.OutputLimit {
		return "", fmt.Errorf("outputs are %d bytes, over the %d-byte limit", len(b), rx.e.cfg.OutputLimit)
	}
	return string(b), nil
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
