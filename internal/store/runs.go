package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const runCols = `id, workflow_id, workflow_name, workflow_version, status, trigger_kind, input_json, root_run_id, parent_run_id, depth, rerun_of, library_hash, claimed_by, claimed_at, heartbeat_at, started_at, finished_at, cancel_requested_at, error, created_at, idempotency_key, concurrency, session_id, delivery_id, actor`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var parent, rerun, lib, claimedBy, idem, session, delivery sql.NullString
	var claimedAt, heartbeat, started, finished, cancel sql.NullString
	var created string
	err := sc.Scan(&r.ID, &r.WorkflowID, &r.WorkflowName, &r.WorkflowVersion, &r.Status, &r.TriggerKind, &r.TriggerJSON, &r.RootRunID,
		&parent, &r.Depth, &rerun, &lib, &claimedBy, &claimedAt, &heartbeat, &started, &finished, &cancel, &r.Error, &created,
		&idem, &r.Concurrency, &session, &delivery, &r.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.ParentRunID, r.RerunOf, r.LibraryHash, r.ClaimedBy = parent.String, rerun.String, lib.String, claimedBy.String
	r.IdempotencyKey, r.SessionID, r.DeliveryID = idem.String, session.String, delivery.String
	r.ClaimedAt, r.HeartbeatAt, r.StartedAt = parseTSPtr(claimedAt), parseTSPtr(heartbeat), parseTSPtr(started)
	r.FinishedAt, r.CancelRequestedAt = parseTSPtr(finished), parseTSPtr(cancel)
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
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO runs (`+runCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			r.ID, r.WorkflowID, r.WorkflowName, r.WorkflowVersion, r.Status, r.TriggerKind, r.TriggerJSON, r.RootRunID,
			nullStr(r.ParentRunID), r.Depth, nullStr(r.RerunOf), nullStr(r.LibraryHash), nullStr(r.ClaimedBy),
			tsPtr(r.ClaimedAt), tsPtr(r.HeartbeatAt), tsPtr(r.StartedAt), tsPtr(r.FinishedAt), tsPtr(r.CancelRequestedAt),
			r.Error, ts(r.CreatedAt), nullStr(r.IdempotencyKey), r.Concurrency, nullStr(r.SessionID), nullStr(r.DeliveryID), r.Actor); err != nil {
			return err
		}
		for _, st := range steps {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO step_runs (run_id, step_id, idx, kind, status) VALUES (?,?,?,?,?)`),
				r.ID, st.StepID, st.Idx, st.Kind, StepPending); err != nil {
				return err
			}
		}
		return nil
	})
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
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

// ActiveRuns returns a workflow's queued and running runs, oldest first.
func (s *Store) ActiveRuns(ctx context.Context, workflowID string) ([]*Run, error) {
	return s.listRuns(ctx, `SELECT `+runCols+` FROM runs WHERE workflow_id = ? AND status IN (?, ?) ORDER BY created_at, id`,
		workflowID, RunQueued, RunRunning)
}

// ClaimRun atomically takes the oldest claimable queued run. A run whose
// policy is "queue" waits while another run of the same workflow is
// running. It returns ErrNotFound when nothing is claimable.
func (s *Store) ClaimRun(ctx context.Context, runnerID string) (*Run, error) {
	now := ts(Now())
	var id string
	err := s.queryRow(ctx, `UPDATE runs SET status = ?, claimed_by = ?, claimed_at = ?, heartbeat_at = ?, started_at = ?
		WHERE id = (
			SELECT r.id FROM runs r
			WHERE r.status = ?
			  AND NOT (r.concurrency = 'queue' AND EXISTS (
			      SELECT 1 FROM runs o WHERE o.workflow_id = r.workflow_id AND o.status = ?))
			ORDER BY r.created_at, r.id LIMIT 1)
		AND status = ?
		RETURNING id`, RunRunning, runnerID, now, now, now, RunQueued, RunRunning, RunQueued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetRun(ctx, id)
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
	res, err := s.exec(ctx, `UPDATE runs SET status = ?, error = ?, finished_at = ? WHERE id = ?`, status, errMsg, ts(Now()), runID)
	if err != nil {
		return err
	}
	return affected(res)
}

// RequestCancel marks a run for cancellation. Queued runs are cancelled
// immediately; running runs are flagged for the engine to stop.
func (s *Store) RequestCancel(ctx context.Context, runID string) (*Run, error) {
	now := ts(Now())
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE runs SET status = ?, finished_at = ?, cancel_requested_at = ? WHERE id = ? AND status = ?`),
			RunCancelled, now, now, runID, RunQueued)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			_, err := tx.ExecContext(ctx, s.rebind(`UPDATE step_runs SET status = ?, finished_at = ? WHERE run_id = ? AND status = ?`),
				StepCancelled, now, runID, StepPending)
			return err
		}
		res, err = tx.ExecContext(ctx, s.rebind(`UPDATE runs SET cancel_requested_at = ? WHERE id = ? AND status = ? AND cancel_requested_at IS NULL`), now, runID, RunRunning)
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
	})
	if err != nil {
		return nil, err
	}
	return s.GetRun(ctx, runID)
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

const stepCols = `run_id, step_id, idx, kind, status, attempt, session_id, container_id, exit_code, outputs_json, log_path, log_size, log_lines, started_at, finished_at, error`

func scanStep(sc interface{ Scan(...any) error }) (*StepRun, error) {
	var st StepRun
	var exit sql.NullInt64
	var started, finished sql.NullString
	err := sc.Scan(&st.RunID, &st.StepID, &st.Idx, &st.Kind, &st.Status, &st.Attempt, &st.SessionID, &st.ContainerID, &exit,
		&st.OutputsJSON, &st.LogPath, &st.LogSize, &st.LogLines, &started, &finished, &st.Error)
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
	res, err := s.exec(ctx, `UPDATE step_runs SET status = ?, attempt = ?, session_id = ?, container_id = ?, exit_code = ?, outputs_json = ?,
		log_path = ?, log_size = ?, log_lines = ?, started_at = ?, finished_at = ?, error = ?
		WHERE run_id = ? AND step_id = ?`,
		st.Status, st.Attempt, st.SessionID, st.ContainerID, exit, st.OutputsJSON, st.LogPath, st.LogSize, st.LogLines,
		tsPtr(st.StartedAt), tsPtr(st.FinishedAt), st.Error, st.RunID, st.StepID)
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
