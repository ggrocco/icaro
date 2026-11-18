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
	"icaro/internal/workflow/events"
)

// Approval, wait and await steps (ADRs 0030, 0032) run nothing: the
// scheduler starts them, checks them on every pass, and parks the run
// when they are all that is left, so a pending approval or event wait
// holds no engine slot.

// startPause makes an approval, wait or await step start waiting.
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
	var a *store.StepAwait
	if st.Kind() == workflow.KindAwait {
		if a = rx.openAwait(ctx, st); a == nil {
			return
		}
	}
	now := time.Now()
	row.Status, row.Attempt, row.StartedAt, row.Error = store.StepWaiting, 1, &now, ""
	rx.persist(ctx, row)
	if a != nil && a.State == store.AwaitWaiting {
		rx.lookBack(ctx, a)
	}
}

// openAwait renders an await step's key and commits its waiting row. It
// settles the step and returns nil when either fails.
func (rx *runExec) openAwait(ctx context.Context, st *workflow.StepSpec) *store.StepAwait {
	row := rx.rows[st.ID]
	env := rx.baseEnv(1)
	env["steps"] = rx.stepsEnv()
	delete(env, "secrets") // the key is stored and displayed
	key, err := expr.RenderString(st.Await.Key, env)
	if err != nil {
		rx.settle(ctx, row, store.StepFailed, "await key: "+err.Error())
		return nil
	}
	if key == "" {
		rx.settle(ctx, row, store.StepFailed, "await key rendered empty")
		return nil
	}
	d, _ := time.ParseDuration(st.Timeout) // validated: required
	a, err := rx.e.d.Store.OpenAwait(ctx, &store.StepAwait{
		RunID: rx.run.ID, StepID: st.ID, Integration: st.Await.Integration, Event: st.Await.Event,
		Workflow: rx.run.WorkflowName, Key: key, Match: st.Await.Match, Filter: st.Await.Filter,
		Deadline: time.Now().Add(d),
	})
	if err != nil {
		rx.settle(ctx, row, store.StepFailed, "open await: "+err.Error())
		return nil
	}
	return a
}

// genericWebhook is the integration whose events correlate only with
// runs of the workflow whose URL token received them (ADR 0032).
const genericWebhook = "webhook"

// lookBack claims the oldest event received since the run started that
// correlates with a just-opened await. The waiting row is committed
// first, and the pipeline persists events before it reads waiting rows,
// so an event landing meanwhile is seen by one side or the other; the
// compare-and-set makes sure it is used once.
func (rx *runExec) lookBack(ctx context.Context, a *store.StepAwait) {
	since := rx.run.CreatedAt
	if rx.run.StartedAt != nil {
		since = *rx.run.StartedAt
	}
	workflowOnly := ""
	if a.Integration == genericWebhook {
		workflowOnly = a.Workflow
	}
	evs, err := rx.e.d.Store.ListDeliveryEventsSince(ctx, a.Integration, a.Event, workflowOnly, since)
	if err != nil {
		// Later deliveries still resume the step; only the lookback is lost.
		rx.e.log.Error("await lookback", "run", rx.run.ID, "step", a.StepID, "err", err)
		return
	}
	c := workflow.NewCorrelator()
	for _, ev := range evs {
		payload := map[string]any{}
		_ = json.Unmarshal([]byte(ev.PayloadJSON), &payload)
		ok, err := c.Correlates(a.Match, a.Filter, a.Key, payload)
		if err != nil {
			rx.e.log.Warn("await correlate", "run", rx.run.ID, "step", a.StepID, "delivery", ev.DeliveryID, "err", err)
			continue
		}
		if !ok {
			continue
		}
		_, err = rx.e.d.Store.ResolveAwait(ctx, rx.run.ID, a.StepID, store.AwaitMatched, ev.DeliveryID, ev.PayloadJSON, "integration:"+a.Integration)
		if err != nil && !errors.Is(err, store.ErrConflict) {
			rx.e.log.Error("await claim", "run", rx.run.ID, "step", a.StepID, "err", err)
		}
		return // matched here, or the pipeline matched first
	}
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
	case workflow.KindAwait:
		return rx.checkAwait(ctx, st)
	}
	return false, nil
}

// checkAwait settles an await step once it was matched, cancelled or its
// deadline passed; otherwise it is next due at its deadline.
func (rx *runExec) checkAwait(ctx context.Context, st *workflow.StepSpec) (bool, *time.Time) {
	row := rx.rows[st.ID]
	a, err := rx.e.d.Store.GetAwait(ctx, rx.run.ID, st.ID)
	if err != nil {
		rx.finishStep(ctx, row, store.StepFailed, nil, "load await: "+err.Error())
		return true, nil
	}
	if a.State == store.AwaitWaiting {
		if time.Now().Before(a.Deadline) {
			return false, &a.Deadline
		}
		// The deadline passed. An event that lands first wins, so read
		// back whatever was recorded.
		b, err := rx.e.d.Store.ResolveAwait(ctx, rx.run.ID, st.ID, store.AwaitExpired, "", "", "")
		if errors.Is(err, store.ErrConflict) {
			b, err = rx.e.d.Store.GetAwait(ctx, rx.run.ID, st.ID)
		}
		if err != nil {
			rx.e.log.Error("expire await", "run", rx.run.ID, "step", st.ID, "err", err)
			return false, nil
		}
		a = b
	}
	switch a.State {
	case store.AwaitMatched:
		payload := map[string]any{}
		_ = json.Unmarshal([]byte(a.PayloadJSON), &payload)
		rx.finishStep(ctx, row, store.StepSucceeded, workflow.AwaitOutputValues(payload, a.DeliveryID), "")
	case store.AwaitExpired:
		rx.finishStep(ctx, row, store.StepFailed, nil,
			fmt.Sprintf("no matching %s.%s before %s", a.Integration, a.Event, a.Deadline.UTC().Format(time.RFC3339)))
	default:
		rx.finishStep(ctx, row, store.StepFailed, nil, "await "+a.State)
	}
	return true, nil
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
// or a cancel request). A decision or matched event recorded while the
// run was still running set no wake time, so it is looked for again once
// parked.
func (rx *runExec) park(ctx context.Context, due *time.Time) error {
	if err := rx.e.d.Store.ParkRun(ctx, rx.run.ID, due); err != nil {
		return fmt.Errorf("park run: %w", err)
	}
	for _, id := range rx.plan.Order {
		if rx.rows[id].Status != store.StepWaiting || !rx.resolved(ctx, id) {
			continue
		}
		return rx.e.d.Store.WakeRun(ctx, rx.run.ID)
	}
	rx.e.publish(events.Event{Type: events.TypeRun, RunID: rx.run.ID, Status: store.RunWaiting})
	return nil
}

// resolved reports whether a waiting approval was decided, or a waiting
// await matched, without the engine having seen it yet.
func (rx *runExec) resolved(ctx context.Context, id string) bool {
	switch rx.spec.Step(id).Kind() {
	case workflow.KindApproval:
		a, err := rx.e.d.Store.GetApproval(ctx, rx.run.ID, id)
		return err == nil && a.Decision != ""
	case workflow.KindAwait:
		a, err := rx.e.d.Store.GetAwait(ctx, rx.run.ID, id)
		return err == nil && a.State != store.AwaitWaiting
	}
	return false
}
