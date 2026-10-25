package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"icaro/internal/expr"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

// errShutdown marks an attempt abandoned because the daemon is stopping.
var errShutdown = errors.New("daemon shutting down")

// runStep executes one step with its retry policy. It owns row until it
// returns, and persists every state change itself.
func (rx *runExec) runStep(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, env expr.Env, resume bool) stepResult {
	maxAttempts := st.Retry + 1
	first := row.Attempt + 1
	if resume {
		outputs, err := rx.reattach(ctx, st, row, env)
		if done, res := rx.afterAttempt(ctx, st, row, row.Attempt, maxAttempts, outputs, err); done {
			return res
		}
	}
	for attempt := first; attempt <= maxAttempts; attempt++ {
		outputs, err := rx.attempt(ctx, st, row, env, attempt, false)
		if done, res := rx.afterAttempt(ctx, st, row, attempt, maxAttempts, outputs, err); done {
			return res
		}
		delay := rx.e.cfg.RetryBase << (attempt - 1)
		if delay > time.Minute {
			delay = time.Minute
		}
		select {
		case <-ctx.Done():
			if !cancelled(ctx) {
				return stepResult{id: st.ID, shutdown: true}
			}
			return rx.finishStep(context.WithoutCancel(ctx), row, store.StepCancelled, nil, "cancelled")
		case <-time.After(delay):
		}
	}
	return rx.finishStep(ctx, row, store.StepFailed, nil, row.Error)
}

// afterAttempt decides what an attempt's outcome means. done=false means
// "retry".
func (rx *runExec) afterAttempt(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, attempt, maxAttempts int, outputs map[string]any, err error) (bool, stepResult) {
	fctx := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		return true, rx.finishStep(fctx, row, store.StepSucceeded, outputs, "")
	case errors.Is(err, errShutdown) || (ctx.Err() != nil && !cancelled(ctx)):
		return true, stepResult{id: st.ID, shutdown: true}
	case cancelled(ctx):
		return true, rx.finishStep(fctx, row, store.StepCancelled, nil, "cancelled")
	case attempt >= maxAttempts:
		return true, rx.finishStep(fctx, row, store.StepFailed, outputs, err.Error())
	}
	row.Status, row.Error = store.StepRunning, fmt.Sprintf("attempt %d failed: %v; retrying", attempt, err)
	rx.persist(fctx, row)
	rx.e.log.Warn("step attempt failed, retrying", "run", rx.run.ID, "step", st.ID, "attempt", attempt, "err", err)
	return false, stepResult{}
}

func (rx *runExec) finishStep(ctx context.Context, row *store.StepRun, status string, outputs map[string]any, msg string) stepResult {
	now := time.Now()
	row.Status, row.Error, row.FinishedAt = status, msg, &now
	if row.StartedAt == nil {
		row.StartedAt = &now
	}
	if status != store.StepSucceeded {
		outputs = nil
	}
	res := stepResult{id: row.StepID, status: status, err: msg, outputs: outputs}
	if outputs != nil {
		row.OutputsJSON = mustJSON(outputs)
	} else {
		row.OutputsJSON = "{}"
	}
	rx.persist(ctx, row)
	return res
}

func (rx *runExec) persist(ctx context.Context, row *store.StepRun) {
	if err := rx.e.d.Store.UpdateStepRun(ctx, row); err != nil {
		rx.e.log.Error("persist step", "run", rx.run.ID, "step", row.StepID, "err", err)
	}
	rx.e.publish(events.Event{Type: events.TypeStep, RunID: rx.run.ID, Step: row.StepID, Attempt: row.Attempt, Status: row.Status})
}

// acquire takes a global step slot.
func (rx *runExec) acquire(ctx context.Context) (func(), error) {
	select {
	case rx.e.slots <- struct{}{}:
		return func() { <-rx.e.slots }, nil
	case <-ctx.Done():
		if cancelled(ctx) {
			return nil, errCancelled
		}
		return nil, errShutdown
	}
}

// attempt runs one attempt of a step.
func (rx *runExec) attempt(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, base expr.Env, attempt int, reattach bool) (map[string]any, error) {
	release, err := rx.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	env := expr.Env{}
	for k, v := range base {
		env[k] = v
	}
	run := map[string]any{}
	for k, v := range base["run"].(map[string]any) {
		run[k] = v
	}
	run["attempt"] = float64(attempt)
	env["run"] = run
	switch st.Kind() {
	case workflow.KindImage:
		return rx.imageAttempt(ctx, st, row, env, attempt, reattach)
	case workflow.KindUses:
		return rx.usesAttempt(ctx, st, row, env, attempt)
	}
	return nil, fmt.Errorf("step %s has no image or uses", st.ID)
}

// reattach resumes a step that was running when the daemon stopped.
func (rx *runExec) reattach(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, env expr.Env) (map[string]any, error) {
	if st.Kind() != workflow.KindImage {
		// An in-daemon action cannot be resumed; it ran at most once and is
		// retried per policy (at-least-once, ADR 0013).
		return nil, errors.New("interrupted by a daemon restart")
	}
	if row.Attempt < 1 {
		return nil, errors.New("interrupted by a daemon restart before the step started")
	}
	return rx.attempt(ctx, st, row, env, row.Attempt, true)
}

func mustJSON(v any) string {
	b, err := jsonMarshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// usesAttempt runs an integration action in the daemon.
func (rx *runExec) usesAttempt(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, env expr.Env, attempt int) (map[string]any, error) {
	now := time.Now()
	row.Status, row.Attempt, row.Error, row.StartedAt, row.FinishedAt = store.StepRunning, attempt, "", &now, nil
	row.LogPath, row.SessionID = rx.e.logPath(rx.run.ID, st.ID, attempt), rx.ws.SessionID
	rx.persist(ctx, row)

	lc, err := rx.e.openLog(ctx, rx, row, attempt, false)
	if err != nil {
		return nil, err
	}
	defer lc.close()
	if rx.e.d.Actions == nil {
		err := fmt.Errorf("no integration action executor is configured; cannot run %s", st.Uses)
		lc.line("error: " + err.Error())
		return nil, err
	}
	in, err := expr.EvalValue(map[string]any(st.Input), env)
	if err != nil {
		lc.line("error: input: " + err.Error())
		return nil, fmt.Errorf("input: %w", err)
	}
	input, _ := in.(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	lc.line(fmt.Sprintf("uses %s (attempt %d)", st.Uses, attempt))
	tctx, cancel := context.WithTimeout(ctx, rx.e.stepTimeout(st))
	defer cancel()
	outputs, err := rx.e.d.Actions.Execute(tctx, st.Uses, input)
	if err != nil {
		if ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", rx.e.stepTimeout(st))
		}
		lc.line("error: " + err.Error())
		return nil, err
	}
	// The executor already redacts responses; the run's redactor also
	// covers values it does not know (an image step's secrets).
	outputs, _ = redactValue(rx.redact, outputs).(map[string]any)
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	lc.line(fmt.Sprintf("ok; outputs: %v", sortStrings(keys)))
	return outputs, nil
}

func (e *Engine) stepTimeout(st *workflow.StepSpec) time.Duration {
	if st.Timeout != "" {
		if d, err := time.ParseDuration(st.Timeout); err == nil && d > 0 && d < e.cfg.StepTimeout {
			return d
		}
	}
	return e.cfg.StepTimeout
}
