package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const runCols = `id, workflow_id, workflow_name, workflow_version, status, trigger_kind, input_json, root_run_id, parent_run_id, depth, rerun_of, library_hash, claimed_by, claimed_at, heartbeat_at, started_at, finished_at, cancel_requested_at, error, created_at, idempotency_key, concurrency, session_id, delivery_id, actor, wake_at, outputs_json`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var parent, rerun, lib, claimedBy, idem, session, delivery sql.NullString
	var claimedAt, heartbeat, started, finished, cancel, wake sql.NullString
	var created string
	err := sc.Scan(&r.ID, &r.WorkflowID, &r.WorkflowName, &r.WorkflowVersion, &r.Status, &r.TriggerKind, &r.TriggerJSON, &r.RootRunID,
		&parent, &r.Depth, &rerun, &lib, &claimedBy, &claimedAt, &heartbeat, &started, &finished, &cancel, &r.Error, &created,
		&idem, &r.Concurrency, &session, &delivery, &r.Actor, &wake, &r.OutputsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.ParentRunID, r.RerunOf, r.LibraryHash, r.ClaimedBy = parent.String, rerun.String, lib.String, claimedBy.String
	r.IdempotencyKey, r.SessionID, r.DeliveryID = idem.String, session.String, delivery.String
	r.ClaimedAt, r.HeartbeatAt, r.StartedAt = parseTSPtr(claimedAt), parseTSPtr(heartbeat), parseTSPtr(started)
	r.FinishedAt, r.CancelRequestedAt, r.WakeAt = parseTSPtr(finished), parseTSPtr(cancel), parseTSPtr(wake)
	r.CreatedAt = parseTS(created)
	return &r, nil
}

// StepInit is a step row created with its run.
type StepInit struct {
	StepID string
	Idx    int
	Kind   string
}

// CreateRun inserts a queued run and its pending steps in one
// transaction. ID, RootRunID and CreatedAt are filled in when empty. A
// duplicate IdempotencyKey returns ErrConflict.
func (s *Store) CreateRun(ctx context.Context, r *Run, steps []StepInit) error {
	s.fillRun(r)
	err := s.tx(ctx, func(tx *sql.Tx) error { return s.insertRun(ctx, tx, r, steps) })
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// PolicyOutcome is what EnqueueRun's concurrency policy did.
type PolicyOutcome struct {
	// Blocking is the active run that made a forbid enqueue skip; no run
	// was created.
	Blocking *Run
	// Replaced are the runs a replace enqueue cancelled (queued ones) or
	// flagged for cancellation (running ones), as they were before.
	Replaced []*Run
}

// EnqueueRun inserts r like CreateRun, applying its workflow's
// concurrency policy (r.Concurrency) in the same transaction, so two
// concurrent enqueues cannot both see "no active run": forbid skips while
// a run is queued or running; replace cancels queued runs and flags
// running ones, and the new run then waits in ClaimRun until they stop.
// The transaction first takes the workflow row's write lock (a no-op
// UPDATE, portable to SQLite's single writer and Postgres row locks); a
// workflow deleted meanwhile returns ErrNotFound.
func (s *Store) EnqueueRun(ctx context.Context, r *Run, steps []StepInit) (PolicyOutcome, error) {
	return s.enqueue(ctx, r, steps, nil)
}

// CallBooking makes an enqueue the child run of a call step (ADR 0038).
type CallBooking struct {
	// ParentRunID and StepID are the call step; it records the child's
	// id and version in the enqueue's transaction.
	ParentRunID string
	StepID      string
	// MaxRunsPerRoot bounds the runs, retries included, that share the
	// child's root run (workflows.max_runs_per_root).
	MaxRunsPerRoot int
}

// ErrRootBudget means a call would exceed workflows.max_runs_per_root.
var ErrRootBudget = errors.New("the root run's budget of runs is spent")

// EnqueueCall is EnqueueRun for a call step's child: r carries the
// lineage (RootRunID, ParentRunID, Depth). In one transaction it checks
// the root's run budget, applies r's concurrency policy, inserts the run
// and books it on the parent's step, so a restart between enqueue and
// bookkeeping cannot lose or duplicate the child.
func (s *Store) EnqueueCall(ctx context.Context, r *Run, steps []StepInit, b CallBooking) (PolicyOutcome, error) {
	return s.enqueue(ctx, r, steps, &b)
}

func (s *Store) enqueue(ctx context.Context, r *Run, steps []StepInit, b *CallBooking) (PolicyOutcome, error) {
	s.fillRun(r)
	var out PolicyOutcome
	err := s.tx(ctx, func(tx *sql.Tx) error {
		out = PolicyOutcome{}
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE workflows SET version = version WHERE id = ?`), r.WorkflowID)
		if err != nil {
			return err
		}
		if err := affected(res); err != nil {
			return err
		}
		if b != nil {
			var n int
			if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM runs WHERE root_run_id = ?`), r.RootRunID).Scan(&n); err != nil {
				return err
			}
			if n >= b.MaxRunsPerRoot {
				return ErrRootBudget
			}
		}
		if r.Concurrency == "forbid" || r.Concurrency == "replace" {
			active, err := s.activeRunsTx(ctx, tx, r.WorkflowID)
			if err != nil {
				return err
			}
			if r.Concurrency == "forbid" && len(active) > 0 {
				out.Blocking = active[0]
				return nil
			}
			for _, old := range active {
				if old.ParentRunID != "" {
					// A call step waits on it (ADR 0038): replace leaves
					// it alone, and the new run waits for it in ClaimRun.
					continue
				}
				if err := s.requestCancelTx(ctx, tx, old.ID); err != nil {
					return err
				}
				out.Replaced = append(out.Replaced, old)
			}
		}
		if err := s.insertRun(ctx, tx, r, steps); err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		res, err = tx.ExecContext(ctx, s.rebind(`UPDATE step_runs SET child_run_id = ?, child_version = ? WHERE run_id = ? AND step_id = ?`),
			r.ID, r.WorkflowVersion, b.ParentRunID, b.StepID)
		if err != nil {
			return err
		}
		return affected(res)
	})
	if isUniqueViolation(err) {
		return PolicyOutcome{}, ErrConflict
	}
	return out, err
}

func (s *Store) activeRunsTx(ctx context.Context, tx *sql.Tx, workflowID string) ([]*Run, error) {
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT `+runCols+` FROM runs WHERE workflow_id = ? AND status IN (?, ?, ?) ORDER BY created_at, id`),
		workflowID, RunQueued, RunRunning, RunWaiting)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) fillRun(r *Run) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.RootRunID == "" {
		r.RootRunID = r.ID
	}
	if r.Status == "" {
		r.Status = RunQueued
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = Now()
	}
	if r.TriggerJSON == "" {
		r.TriggerJSON = "{}"
	}
	if r.Concurrency == "" {
		r.Concurrency = "allow"
	}
	if r.OutputsJSON == "" {
		r.OutputsJSON = "{}"
	}
}

func (s *Store) insertRun(ctx context.Context, tx *sql.Tx, r *Run, steps []StepInit) error {
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO runs (`+runCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		r.ID, r.WorkflowID, r.WorkflowName, r.WorkflowVersion, r.Status, r.TriggerKind, r.TriggerJSON, r.RootRunID,
		nullStr(r.ParentRunID), r.Depth, nullStr(r.RerunOf), nullStr(r.LibraryHash), nullStr(r.ClaimedBy),
		tsPtr(r.ClaimedAt), tsPtr(r.HeartbeatAt), tsPtr(r.StartedAt), tsPtr(r.FinishedAt), tsPtr(r.CancelRequestedAt),
		r.Error, ts(r.CreatedAt), nullStr(r.IdempotencyKey), r.Concurrency, nullStr(r.SessionID), nullStr(r.DeliveryID), r.Actor, tsPtr(r.WakeAt), r.OutputsJSON); err != nil {
		return err
	}
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO step_runs (run_id, step_id, idx, kind, status) VALUES (?,?,?,?,?)`),
			r.ID, st.StepID, st.Idx, st.Kind, StepPending); err != nil {
			return err
		}
	}
	return nil
}

// GetRun fetches a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.queryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
}

// GetRunByIdempotencyKey fetches the run created for a key.
func (s *Store) GetRunByIdempotencyKey(ctx context.Context, key string) (*Run, error) {
	return scanRun(s.queryRow(ctx, `SELECT `+runCols+` FROM runs WHERE idempotency_key = ?`, key))
}

func (s *Store) listRuns(ctx context.Context, q string, args ...any) ([]*Run, error) {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRuns returns runs newest first, filtered.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	var where []string
	var args []any
	if f.WorkflowID != "" {
		where = append(where, "workflow_id = ?")
		args = append(args, f.WorkflowID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.RootRunID != "" {
		where = append(where, "root_run_id = ?")
		args = append(args, f.RootRunID)
	}
	q := `SELECT ` + runCols + ` FROM runs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT %d", limit)
	return s.listRuns(ctx, q, args...)
}

// ListRunsByStatus returns runs in a status, oldest first.
func (s *Store) ListRunsByStatus(ctx context.Context, status string) ([]*Run, error) {
	return s.listRuns(ctx, `SELECT `+runCols+` FROM runs WHERE status = ? ORDER BY created_at, id`, status)
}

// ActiveRuns returns a workflow's queued, running and waiting runs,
// oldest first.
func (s *Store) ActiveRuns(ctx context.Context, workflowID string) ([]*Run, error) {
	return s.listRuns(ctx, `SELECT `+runCols+` FROM runs WHERE workflow_id = ? AND status IN (?, ?, ?) ORDER BY created_at, id`,
		workflowID, RunQueued, RunRunning, RunWaiting)
}

// ActiveChildren returns the queued, running and waiting runs whose
// parent is runID (ADR 0038).
func (s *Store) ActiveChildren(ctx context.Context, runID string) ([]*Run, error) {
	return s.listRuns(ctx, `SELECT `+runCols+` FROM runs WHERE parent_run_id = ? AND status IN (?, ?, ?) ORDER BY created_at, id`,
		runID, RunQueued, RunRunning, RunWaiting)
}

// ActiveDescendants returns every queued, running and waiting run below
// runID in its tree, parents before children.
func (s *Store) ActiveDescendants(ctx context.Context, runID string) ([]*Run, error) {
	var out []*Run
	queue := []string{runID}
	for len(queue) > 0 {
		kids, err := s.ActiveChildren(ctx, queue[0])
		if err != nil {
			return nil, err
		}
		queue = queue[1:]
		for _, k := range kids {
			out = append(out, k)
			queue = append(queue, k.ID)
		}
	}
	return out, nil
}

// ClaimRun atomically takes the oldest claimable queued run. A run whose
// policy is "queue" or "replace" waits while another run of the same
// workflow is running or waiting: for replace, that is the run it
// cancelled, whose containers are still being killed. It returns
// ErrNotFound when nothing is claimable.
func (s *Store) ClaimRun(ctx context.Context, runnerID string) (*Run, error) {
	now := ts(Now())
	var id string
	err := s.queryRow(ctx, `UPDATE runs SET status = ?, claimed_by = ?, claimed_at = ?, heartbeat_at = ?, started_at = ?
		WHERE id = (
			SELECT r.id FROM runs r
			WHERE r.status = ?
			  AND NOT (r.concurrency IN ('queue', 'replace') AND EXISTS (
			      SELECT 1 FROM runs o WHERE o.workflow_id = r.workflow_id AND o.status IN (?, ?)))
			ORDER BY r.created_at, r.id LIMIT 1)
		AND status = ?
		RETURNING id`, RunRunning, runnerID, now, now, now, RunQueued, RunRunning, RunWaiting, RunQueued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetRun(ctx, id)
}

// ClaimDueRun atomically takes the waiting run that has been due the
// longest: its wake time has passed, or someone asked to cancel it. The
// run keeps its start time and run session; the engine resumes it.
// It returns ErrNotFound when no waiting run is due.
func (s *Store) ClaimDueRun(ctx context.Context, runnerID string) (*Run, error) {
	now := ts(Now())
	var id string
	err := s.queryRow(ctx, `UPDATE runs SET status = ?, claimed_by = ?, claimed_at = ?, heartbeat_at = ?, wake_at = NULL
		WHERE id = (
			SELECT id FROM runs
			WHERE status = ? AND ((wake_at IS NOT NULL AND wake_at <= ?) OR (cancel_requested_at IS NOT NULL AND wake_at IS NULL))
			ORDER BY COALESCE(wake_at, cancel_requested_at), id LIMIT 1)
		AND status = ?
		RETURNING id`, RunRunning, runnerID, now, now, RunWaiting, now, RunWaiting).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetRun(ctx, id)
}

// ParkRun parks a running run until wakeAt (nil: until a decision or a
// cancel request), giving its engine slot back (ADR 0030).
func (s *Store) ParkRun(ctx context.Context, runID string, wakeAt *time.Time) error {
	res, err := s.exec(ctx, `UPDATE runs SET status = ?, wake_at = ? WHERE id = ? AND status = ?`,
		RunWaiting, tsPtr(wakeAt), runID, RunRunning)
	if err != nil {
		return err
	}
	return affected(res)
}

// WakeRun makes a waiting run due now.
func (s *Store) WakeRun(ctx context.Context, runID string) error {
	_, err := s.exec(ctx, `UPDATE runs SET wake_at = ? WHERE id = ? AND status = ?`, ts(Now()), runID, RunWaiting)
	return err
}

// SetRunSession records the run session backing a run (ADR 0010).
func (s *Store) SetRunSession(ctx context.Context, runID, sessionID string) error {
	res, err := s.exec(ctx, `UPDATE runs SET session_id = ? WHERE id = ?`, nullStr(sessionID), runID)
	if err != nil {
		return err
	}
	return affected(res)
}

// Heartbeat refreshes the daemon's liveness mark on a run.
func (s *Store) Heartbeat(ctx context.Context, runID string) error {
	_, err := s.exec(ctx, `UPDATE runs SET heartbeat_at = ? WHERE id = ?`, ts(Now()), runID)
	return err
}

// FinishRun records the terminal status of a run.
func (s *Store) FinishRun(ctx context.Context, runID, status, errMsg string) error {
	return s.FinishRunOutputs(ctx, runID, status, errMsg, "")
}

// FinishRunOutputs records the terminal status of a run and its workflow
// outputs ("" = none). In the same transaction it wakes the run's parent,
// so a call step waiting on it is looked at again at once (ADR 0038).
func (s *Store) FinishRunOutputs(ctx context.Context, runID, status, errMsg, outputsJSON string) error {
	if outputsJSON == "" {
		outputsJSON = "{}"
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE runs SET status = ?, error = ?, finished_at = ?, outputs_json = ? WHERE id = ?`),
			status, errMsg, ts(Now()), outputsJSON, runID)
		if err != nil {
			return err
		}
		if err := affected(res); err != nil {
			return err
		}
		return s.wakeParentTx(ctx, tx, runID)
	})
}

// wakeParentTx makes a parked parent of runID due now (ADR 0038). A
// parent that is still running looks at its call steps on its next poll.
func (s *Store) wakeParentTx(ctx context.Context, tx *sql.Tx, runID string) error {
	_, err := tx.ExecContext(ctx, s.rebind(`UPDATE runs SET wake_at = ? WHERE status = ?
		AND id = (SELECT parent_run_id FROM runs WHERE id = ?)`), ts(Now()), RunWaiting, runID)
	return err
}

// RequestCancel marks a run for cancellation. Queued runs are cancelled
// immediately; running runs are flagged for the engine to stop.
func (s *Store) RequestCancel(ctx context.Context, runID string) (*Run, error) {
	if err := s.tx(ctx, func(tx *sql.Tx) error { return s.requestCancelTx(ctx, tx, runID) }); err != nil {
		return nil, err
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) requestCancelTx(ctx context.Context, tx *sql.Tx, runID string) error {
	now := ts(Now())
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE runs SET status = ?, finished_at = ?, cancel_requested_at = ? WHERE id = ? AND status = ?`),
		RunCancelled, now, now, runID, RunQueued)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE step_runs SET status = ?, finished_at = ?, error = ? WHERE run_id = ? AND status = ?`),
			StepCancelled, now, "run cancelled", runID, StepPending); err != nil {
			return err
		}
		return s.wakeParentTx(ctx, tx, runID)
	}
	// Cancelling goes down the tree (ADR 0038): every active child is
	// cancelled, or flagged, before the run itself.
	children, err := tx.QueryContext(ctx, s.rebind(`SELECT id FROM runs WHERE parent_run_id = ? AND status IN (?, ?, ?)`),
		runID, RunQueued, RunRunning, RunWaiting)
	if err != nil {
		return err
	}
	var ids []string
	for children.Next() {
		var id string
		if err := children.Scan(&id); err != nil {
			_ = children.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := children.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.requestCancelTx(ctx, tx, id); err != nil {
			return err
		}
	}
	// A running run is flagged for the engine to stop; a waiting one is
	// also woken, so the engine claims it and cancels it the same way.
	res, err = tx.ExecContext(ctx, s.rebind(`UPDATE runs SET cancel_requested_at = ?,
		wake_at = CASE WHEN status = ? THEN ? ELSE wake_at END
		WHERE id = ? AND status IN (?, ?) AND cancel_requested_at IS NULL`), now, RunWaiting, now, runID, RunRunning, RunWaiting)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM runs WHERE id = ?`), runID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// CountRunsByStatus returns counts keyed by status.
func (s *Store) CountRunsByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.query(ctx, `SELECT status, COUNT(*) FROM runs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

const stepCols = `run_id, step_id, idx, kind, status, attempt, session_id, container_id, exit_code, outputs_json, log_path, log_size, log_lines, started_at, finished_at, error,
	profile, image_digest, input_tokens, output_tokens, cache_read, cache_write, cost_micro, child_run_id, child_version`

func scanStep(sc interface{ Scan(...any) error }) (*StepRun, error) {
	var st StepRun
	var exit sql.NullInt64
	var started, finished sql.NullString
	u := &st.Usage
	err := sc.Scan(&st.RunID, &st.StepID, &st.Idx, &st.Kind, &st.Status, &st.Attempt, &st.SessionID, &st.ContainerID, &exit,
		&st.OutputsJSON, &st.LogPath, &st.LogSize, &st.LogLines, &started, &finished, &st.Error,
		&st.Profile, &st.ImageDigest, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostMicro, &st.ChildRunID, &st.ChildVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exit.Valid {
		v := int(exit.Int64)
		st.ExitCode = &v
	}
	st.StartedAt, st.FinishedAt = parseTSPtr(started), parseTSPtr(finished)
	return &st, nil
}

// GetStepRuns returns the steps of a run in plan order.
func (s *Store) GetStepRuns(ctx context.Context, runID string) ([]*StepRun, error) {
	rows, err := s.query(ctx, `SELECT `+stepCols+` FROM step_runs WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*StepRun
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetStepRun returns one step.
func (s *Store) GetStepRun(ctx context.Context, runID, stepID string) (*StepRun, error) {
	return scanStep(s.queryRow(ctx, `SELECT `+stepCols+` FROM step_runs WHERE run_id = ? AND step_id = ?`, runID, stepID))
}

// UpdateStepRun writes the full mutable state of a step.
func (s *Store) UpdateStepRun(ctx context.Context, st *StepRun) error {
	var exit any
	if st.ExitCode != nil {
		exit = *st.ExitCode
	}
	if st.OutputsJSON == "" {
		st.OutputsJSON = "{}"
	}
	u := st.Usage
	res, err := s.exec(ctx, `UPDATE step_runs SET status = ?, attempt = ?, session_id = ?, container_id = ?, exit_code = ?, outputs_json = ?,
		log_path = ?, log_size = ?, log_lines = ?, started_at = ?, finished_at = ?, error = ?,
		profile = ?, image_digest = ?, input_tokens = ?, output_tokens = ?, cache_read = ?, cache_write = ?, cost_micro = ?,
		child_run_id = ?, child_version = ?
		WHERE run_id = ? AND step_id = ?`,
		st.Status, st.Attempt, st.SessionID, st.ContainerID, exit, st.OutputsJSON, st.LogPath, st.LogSize, st.LogLines,
		tsPtr(st.StartedAt), tsPtr(st.FinishedAt), st.Error,
		st.Profile, st.ImageDigest, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.CostMicro, st.ChildRunID, st.ChildVersion, st.RunID, st.StepID)
	if err != nil {
		return err
	}
	return affected(res)
}

// AddLogChunk records a log index entry.
func (s *Store) AddLogChunk(ctx context.Context, c *LogChunk) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = Now()
	}
	_, err := s.exec(ctx, `INSERT INTO log_chunks (run_id, step_id, attempt, seq, file_offset, line_count, created_at) VALUES (?,?,?,?,?,?,?)`,
		c.RunID, c.StepID, c.Attempt, c.Seq, c.FileOffset, c.LineCount, ts(c.CreatedAt))
	return err
}

// LastLogChunk returns the newest index entry of a step attempt.
func (s *Store) LastLogChunk(ctx context.Context, runID, stepID string, attempt int) (*LogChunk, error) {
	var c LogChunk
	var created string
	err := s.queryRow(ctx, `SELECT run_id, step_id, attempt, seq, file_offset, line_count, created_at FROM log_chunks
		WHERE run_id = ? AND step_id = ? AND attempt = ? ORDER BY seq DESC LIMIT 1`, runID, stepID, attempt).
		Scan(&c.RunID, &c.StepID, &c.Attempt, &c.Seq, &c.FileOffset, &c.LineCount, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = parseTS(created)
	return &c, nil
}

// ListLogChunks returns a step attempt's index entries in order.
func (s *Store) ListLogChunks(ctx context.Context, runID, stepID string, attempt int) ([]*LogChunk, error) {
	rows, err := s.query(ctx, `SELECT run_id, step_id, attempt, seq, file_offset, line_count, created_at FROM log_chunks
		WHERE run_id = ? AND step_id = ? AND attempt = ? ORDER BY seq`, runID, stepID, attempt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*LogChunk
	for rows.Next() {
		var c LogChunk
		var created string
		if err := rows.Scan(&c.RunID, &c.StepID, &c.Attempt, &c.Seq, &c.FileOffset, &c.LineCount, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTS(created)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ActiveWorkflows returns every active workflow.
func (s *Store) ActiveWorkflows(ctx context.Context) ([]*Workflow, error) {
	all, err := s.ListWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, w := range all {
		if w.Status == WorkflowActive {
			out = append(out, w)
		}
	}
	return out, nil
}

// AgentStepUsage is one agent step run's token usage (ADR 0037).
type AgentStepUsage struct {
	RunID, StepID, Workflow, Profile string
	StartedAt                        time.Time
	Usage                            StepUsage
}

// ListAgentStepUsage returns the step runs that recorded token usage and
// started at or after since.
func (s *Store) ListAgentStepUsage(ctx context.Context, since time.Time) ([]*AgentStepUsage, error) {
	rows, err := s.query(ctx, `SELECT sr.run_id, sr.step_id, r.workflow_name, sr.profile, sr.started_at,
		sr.input_tokens, sr.output_tokens, sr.cache_read, sr.cache_write, sr.cost_micro
		FROM step_runs sr JOIN runs r ON r.id = sr.run_id
		WHERE sr.profile <> '' AND sr.started_at IS NOT NULL AND sr.started_at >= ?
		ORDER BY sr.started_at, sr.run_id, sr.step_id`, ts(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentStepUsage
	for rows.Next() {
		var u AgentStepUsage
		var started string
		if err := rows.Scan(&u.RunID, &u.StepID, &u.Workflow, &u.Profile, &started,
			&u.Usage.Input, &u.Usage.Output, &u.Usage.CacheRead, &u.Usage.CacheWrite, &u.Usage.CostMicro); err != nil {
			return nil, err
		}
		u.StartedAt = parseTS(started)
		out = append(out, &u)
	}
	return out, rows.Err()
}
