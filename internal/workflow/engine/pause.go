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

// Approval and wait steps (ADR 0030) run nothing: the scheduler starts
// them, checks them on every pass, and parks the run when they are all
// that is left, so a pending approval holds no engine slot.

// startPause makes an approval or wait step start waiting.
func (rx *runExec) startPause(ctx context.Context, st *workflow.StepSpec) {
	row := rx.rows[st.ID]
	if st.Kind() == workflow.KindApproval {
		msg := "Approve step " + st.ID + "?"
		if st.Approval.Message != "" {
			env := rx.baseEnv(1)
			env["steps"] = rx.stepsEnv()
			delete(env, "secrets") // the message is displayed
			out, err := expr.RenderString(st.Approval.Message, env)
			if err != nil {
				rx.settle(ctx, row, store.StepFailed, "approval message: "+err.Error())
				return
			}
			msg = out
		}
		if _, err := rx.e.d.Store.OpenApproval(ctx, rx.run.ID, st.ID, msg); err != nil {
			rx.settle(ctx, row, store.StepFailed, "open approval: "+err.Error())
			return
		}
	}
	now := time.Now()
	row.Status, row.Attempt, row.StartedAt, row.Error = store.StepWaiting, 1, &now, ""
	rx.persist(ctx, row)
}

// checkPause settles a waiting step whose condition holds. Otherwise it
// returns when the step is next due: a wait's end or an approval's
// deadline; nil means it waits for a decision only.
func (rx *runExec) checkPause(ctx context.Context, st *workflow.StepSpec) (settled bool, due *time.Time) {
	row := rx.rows[st.ID]
	started := time.Now()
	if row.StartedAt != nil {
		started = *row.StartedAt
	}
	switch st.Kind() {
	case workflow.KindWait:
		d, _ := time.ParseDuration(st.Wait) // validated
		until := started.Add(d)
		if !time.Now().Before(until) {
			rx.finishStep(ctx, row, store.StepSucceeded, map[string]any{}, "")
			return true, nil
		}
		return false, &until
	case workflow.KindApproval:
		a, err := rx.e.d.Store.GetApproval(ctx, rx.run.ID, st.ID)
		if err != nil {
			rx.finishStep(ctx, row, store.StepFailed, nil, "load approval: "+err.Error())
			return true, nil
		}
		if a.Decision == "" && st.Timeout != "" {
			d, _ := time.ParseDuration(st.Timeout) // validated
			deadline := started.Add(d)
			if time.Now().Before(deadline) {
				return false, &deadline
			}
			// The deadline passed undecided. A decision that lands first
			// wins, so read back whatever was recorded.
			if b, err := rx.e.d.Store.DecideApproval(ctx, rx.run.ID, st.ID, store.ApprovalExpired, "", "no decision within "+st.Timeout); err == nil {
				a = b
			} else if errors.Is(err, store.ErrConflict) {
				if a, err = rx.e.d.Store.GetApproval(ctx, rx.run.ID, st.ID); err != nil {
					rx.finishStep(ctx, row, store.StepFailed, nil, "load approval: "+err.Error())
					return true, nil
				}
			} else {
				rx.e.log.Error("expire approval", "run", rx.run.ID, "step", st.ID, "err", err)
				return false, nil
			}
		}
		switch a.Decision {
		case store.ApprovalApproved:
			rx.finishStep(ctx, row, store.StepSucceeded, map[string]any{"approved_by": a.Actor, "comment": a.Comment}, "")
		case store.ApprovalRejected:
			msg := "rejected by " + a.Actor
			if a.Comment != "" {
				msg += ": " + a.Comment
			}
			rx.finishStep(ctx, row, store.StepFailed, nil, msg)
		case store.ApprovalExpired:
			rx.finishStep(ctx, row, store.StepFailed, nil, "approval expired: "+a.Comment)
		default:
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

// checkPauses settles every waiting step that can be. It reports whether
// any settled, and the earliest time a remaining one is due (nil: none
// has a time).
func (rx *runExec) checkPauses(ctx context.Context) (settled bool, due *time.Time) {
	for _, id := range rx.plan.Order {
		if rx.rows[id].Status != store.StepWaiting {
			continue
		}
		ok, at := rx.checkPause(ctx, rx.spec.Step(id))
		settled = settled || ok
		if at != nil && (due == nil || at.Before(*due)) {
			due = at
		}
	}
	return settled, due
}

func (rx *runExec) anyWaiting() bool {
	for _, id := range rx.plan.Order {
		if rx.rows[id].Status == store.StepWaiting {
			return true
		}
	}
	return false
}

// park gives the run's engine slot back until due (nil: until a decision
// or a cancel request). A decision recorded while the run was still
// running set no wake time, so it is looked for again once parked.
func (rx *runExec) park(ctx context.Context, due *time.Time) error {
	if err := rx.e.d.Store.ParkRun(ctx, rx.run.ID, due); err != nil {
		return fmt.Errorf("park run: %w", err)
	}
	for _, id := range rx.plan.Order {
		if rx.rows[id].Status != store.StepWaiting || rx.spec.Step(id).Kind() != workflow.KindApproval {
			continue
		}
		if a, err := rx.e.d.Store.GetApproval(ctx, rx.run.ID, id); err == nil && a.Decision != "" {
			return rx.e.d.Store.WakeRun(ctx, rx.run.ID)
		}
	}
	rx.e.publish(events.Event{Type: events.TypeRun, RunID: rx.run.ID, Status: store.RunWaiting})
	return nil
}
