package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ggrocco/icaro/internal/store"
	"github.com/ggrocco/icaro/internal/workflow"
)

// errCancelled marks a run stopped by request.
var errCancelled = errors.New("cancelled")

// runCtx carries the per-run state shared by step executors.
type runCtx struct {
	run     *store.Run
	wf      *workflow.Workflow
	inputs  map[string]any
	steps   map[string]any // name -> {outputs, exit_code, status}
	wsVol   string
	ioVol   string
	resumed bool
}

func (rc *runCtx) templateData(attempt int) map[string]any {
	return map[string]any{
		"inputs": rc.inputs,
		"steps":  rc.steps,
		"run": map[string]any{
			"id": rc.run.ID, "workflow": rc.run.WorkflowName, "attempt": attempt, "trigger": rc.run.TriggerKind,
		},
		"workflow": map[string]any{"name": rc.run.WorkflowName, "version": rc.run.WorkflowVersion},
	}
}

// execute drives one run to a terminal state.
func (r *Runner) execute(parent context.Context, run *store.Run, resumed bool) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	untrack := r.track(run.ID, cancel)
	defer untrack()

	// Heartbeat while executing.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(r.cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = r.store.Heartbeat(hbCtx, run.ID)
				if cur, err := r.store.GetRun(hbCtx, run.ID); err == nil && cur.CancelRequestedAt != nil {
					cancel()
				}
			}
		}
	}()

	status, errMsg := r.executeRun(ctx, run, resumed)
	// Use the parent context for the final write: ctx may be cancelled.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer fcancel()
	if err := r.store.FinishRun(fctx, run.ID, status, errMsg); err != nil {
		r.log.Error("finish run", "run", run.ID, "err", err)
	}
	r.log.Info("run finished", "run", run.ID, "workflow", run.WorkflowName, "status", status, "error", errMsg)
	r.notify(run.ID)
}

func (r *Runner) executeRun(ctx context.Context, run *store.Run, resumed bool) (status, errMsg string) {
	ver, err := r.store.GetWorkflowVersion(ctx, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		return store.RunFailed, fmt.Sprintf("load workflow version: %v", err)
	}
	wf, issues, err := workflow.Parse([]byte(ver.SpecYAML), workflow.Options{})
	if err != nil {
		return store.RunFailed, fmt.Sprintf("parse workflow: %v", err)
	}
	if len(issues) > 0 {
		return store.RunFailed, "workflow is invalid: " + issues.Error()
	}
	inputs := map[string]any{}
	if err := json.Unmarshal([]byte(run.InputJSON), &inputs); err != nil {
		return store.RunFailed, fmt.Sprintf("decode input: %v", err)
	}
	rc := &runCtx{
		run: run, wf: wf, inputs: inputs, steps: map[string]any{},
		wsVol: "icaro-ws-" + strings.ToLower(run.ID), ioVol: "icaro-io-" + strings.ToLower(run.ID), resumed: resumed,
	}
	labels := map[string]string{"icaro.run": run.ID}
	if err := r.docker.CreateVolume(ctx, rc.wsVol, labels); err != nil {
		return store.RunFailed, fmt.Sprintf("create workspace volume: %v", err)
	}
	if err := r.docker.CreateVolume(ctx, rc.ioVol, labels); err != nil {
		return store.RunFailed, fmt.Sprintf("create io volume: %v", err)
	}

	stepRows, err := r.store.GetRunSteps(ctx, run.ID)
	if err != nil {
		return store.RunFailed, fmt.Sprintf("load steps: %v", err)
	}
	if len(stepRows) != len(wf.Steps) {
		return store.RunFailed, fmt.Sprintf("run has %d step rows, workflow has %d steps", len(stepRows), len(wf.Steps))
	}

	status = store.RunSucceeded
	for i := range wf.Steps {
		step := &wf.Steps[i]
		row := stepRows[i]
		switch row.Status {
		case store.StepSucceeded, store.StepSkipped:
			rc.recordStep(step.Name, row)
			continue
		case store.StepFailed:
			if step.ContinueOnError {
				rc.recordStep(step.Name, row)
				continue
			}
			return store.RunFailed, fmt.Sprintf("step %s failed", step.Name)
		}
		if ctx.Err() != nil {
			return store.RunCancelled, errCancelled.Error()
		}
		if err := r.executeStep(ctx, rc, step, row); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, errCancelled) {
				return store.RunCancelled, errCancelled.Error()
			}
			if !step.ContinueOnError {
				r.cleanupVolumes(ctx, rc, false)
				return store.RunFailed, fmt.Sprintf("step %s: %v", step.Name, err)
			}
			status = store.RunSucceeded // continue_on_error keeps the run going
		}
		rc.recordStep(step.Name, row)
		r.notify(run.ID)
	}
	r.cleanupVolumes(ctx, rc, true)
	return status, ""
}

func (rc *runCtx) recordStep(name string, row *store.RunStep) {
	outputs := map[string]any{}
	if row.OutputJSON != "" {
		_ = json.Unmarshal([]byte(row.OutputJSON), &outputs)
	}
	entry := map[string]any{"outputs": outputs, "status": row.Status}
	if row.ExitCode != nil {
		entry["exit_code"] = *row.ExitCode
	}
	rc.steps[name] = entry
}

func (r *Runner) cleanupVolumes(ctx context.Context, rc *runCtx, success bool) {
	if !success && r.cfg.KeepWorkspaceOnFailure {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = r.docker.RemoveVolume(cctx, rc.wsVol)
	_ = r.docker.RemoveVolume(cctx, rc.ioVol)
}

// executeStep runs a step with its retry policy and persists its state.
func (r *Runner) executeStep(ctx context.Context, rc *runCtx, step *workflow.Step, row *store.RunStep) error {
	attempts, backoff := 1, 10*time.Second
	if step.Retry != nil {
		attempts = step.Retry.Attempts
		if step.Retry.Backoff != "" {
			backoff, _ = time.ParseDuration(step.Retry.Backoff)
		}
	}

	// Evaluate the condition once, before the first attempt.
	if row.Status == store.StepPending {
		ok, err := workflow.RenderBool(step.Name+"/if", step.If, rc.templateData(1))
		if err != nil {
			return r.failStep(ctx, row, 1, err)
		}
		if !ok {
			now := time.Now()
			row.Status, row.StartedAt, row.FinishedAt = store.StepSkipped, &now, &now
			return r.store.UpdateRunStep(ctx, row)
		}
	}

	startAttempt := row.Attempt
	if row.Status != store.StepRunning {
		startAttempt++
	}
	for attempt := max(startAttempt, 1); attempt <= attempts; attempt++ {
		resuming := rc.resumed && row.Status == store.StepRunning && row.Attempt == attempt && row.ContainerID != ""
		var err error
		switch step.Kind() {
		case workflow.KindRun:
			err = r.runContainerStep(ctx, rc, step, row, attempt, resuming)
		case workflow.KindHTTP:
			err = errors.New("http steps are not implemented yet")
		default:
			err = errors.New("unknown step kind")
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			_ = r.failStep(context.WithoutCancel(ctx), row, attempt, errCancelled)
			return errCancelled
		}
		if attempt == attempts {
			return r.failStep(ctx, row, attempt, err)
		}
		r.log.Warn("step attempt failed, retrying", "run", rc.run.ID, "step", step.Name, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			_ = r.failStep(context.WithoutCancel(ctx), row, attempt, errCancelled)
			return errCancelled
		case <-time.After(backoff):
		}
	}
	return nil
}

func (r *Runner) failStep(ctx context.Context, row *store.RunStep, attempt int, err error) error {
	now := time.Now()
	row.Status, row.Attempt, row.FinishedAt, row.Error = store.StepFailed, attempt, &now, err.Error()
	if row.StartedAt == nil {
		row.StartedAt = &now
	}
	if uerr := r.store.UpdateRunStep(ctx, row); uerr != nil {
		r.log.Error("persist step failure", "err", uerr)
	}
	return err
}

func (r *Runner) stepTimeout(step *workflow.Step) time.Duration {
	if step.Timeout != "" {
		if d, err := time.ParseDuration(step.Timeout); err == nil && d > 0 && d < r.cfg.StepTimeout {
			return d
		}
	}
	return r.cfg.StepTimeout
}
