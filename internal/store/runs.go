package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const runCols = `id, workflow_id, workflow_name, workflow_version, status, trigger_kind, input_json, root_run_id, parent_run_id, depth, rerun_of, library_hash, claimed_by, claimed_at, heartbeat_at, started_at, finished_at, cancel_requested_at, error, created_at`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var parent, rerun, lib, claimedBy sql.NullString
	var claimedAt, heartbeat, started, finished, cancel sql.NullString
	var created string
	err := sc.Scan(&r.ID, &r.WorkflowID, &r.WorkflowName, &r.WorkflowVersion, &r.Status, &r.TriggerKind, &r.InputJSON, &r.RootRunID,
		&parent, &r.Depth, &rerun, &lib, &claimedBy, &claimedAt, &heartbeat, &started, &finished, &cancel, &r.Error, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.ParentRunID, r.RerunOf, r.LibraryHash, r.ClaimedBy = parent.String, rerun.String, lib.String, claimedBy.String
	r.ClaimedAt, r.HeartbeatAt, r.StartedAt = parseTSPtr(claimedAt), parseTSPtr(heartbeat), parseTSPtr(started)
	r.FinishedAt, r.CancelRequestedAt = parseTSPtr(finished), parseTSPtr(cancel)
	r.CreatedAt = parseTS(created)
	return &r, nil
}

// CreateRun inserts a queued run and its pending steps. ID, RootRunID and
// CreatedAt are filled in when empty.
func (s *Store) CreateRun(ctx context.Context, r *Run, stepNames []string) error {
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
	if r.InputJSON == "" {
		r.InputJSON = "{}"
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO runs (`+runCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			r.ID, r.WorkflowID, r.WorkflowName, r.WorkflowVersion, r.Status, r.TriggerKind, r.InputJSON, r.RootRunID,
			nullStr(r.ParentRunID), r.Depth, nullStr(r.RerunOf), nullStr(r.LibraryHash), nullStr(r.ClaimedBy),
			tsPtr(r.ClaimedAt), tsPtr(r.HeartbeatAt), tsPtr(r.StartedAt), tsPtr(r.FinishedAt), tsPtr(r.CancelRequestedAt),
			r.Error, ts(r.CreatedAt)); err != nil {
			return err
		}
		for i, name := range stepNames {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO run_steps (run_id, idx, name, status) VALUES (?,?,?,?)`), r.ID, i, name, StepPending); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetRun fetches a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.queryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
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

// ClaimRun atomically takes the oldest queued run for a runner. It returns
// ErrNotFound when the queue is empty.
func (s *Store) ClaimRun(ctx context.Context, runnerID string) (*Run, error) {
	now := ts(Now())
	var id string
	err := s.queryRow(ctx, `UPDATE runs SET status = ?, claimed_by = ?, claimed_at = ?, heartbeat_at = ?, started_at = ?
		WHERE id = (SELECT id FROM runs WHERE status = ? ORDER BY created_at, id LIMIT 1) AND status = ?
		RETURNING id`, RunRunning, runnerID, now, now, now, RunQueued, RunQueued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetRun(ctx, id)
}

// Heartbeat refreshes the runner's liveness mark on a run.
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
// immediately; running runs are flagged for the runner to stop.
func (s *Store) RequestCancel(ctx context.Context, runID string) (*Run, error) {
	now := ts(Now())
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE runs SET status = ?, finished_at = ?, cancel_requested_at = ? WHERE id = ? AND status = ?`),
			RunCancelled, now, now, runID, RunQueued)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		res, err = tx.ExecContext(ctx, s.rebind(`UPDATE runs SET cancel_requested_at = ? WHERE id = ? AND status = ? AND cancel_requested_at IS NULL`), now, runID, RunRunning)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Already finished or already requested: not an error, caller sees state.
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

// ListRunsClaimedBy returns running runs held by a runner (for reconcile).
func (s *Store) ListRunsClaimedBy(ctx context.Context, runnerID string) ([]*Run, error) {
	rows, err := s.query(ctx, `SELECT `+runCols+` FROM runs WHERE claimed_by = ? AND status = ? ORDER BY created_at`, runnerID, RunRunning)
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

// ReleaseStaleRuns re-queues running runs whose heartbeat is older than
// maxAge and whose runner is not the caller (crash recovery for other
// runners). Returns the number of runs re-queued.
func (s *Store) ReleaseStaleRuns(ctx context.Context, exceptRunner string, maxAge time.Duration) (int64, error) {
	cutoff := ts(Now().Add(-maxAge))
	res, err := s.exec(ctx, `UPDATE runs SET status = ?, claimed_by = NULL, claimed_at = NULL, heartbeat_at = NULL
		WHERE status = ? AND claimed_by <> ? AND heartbeat_at < ?`, RunQueued, RunRunning, exceptRunner, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const stepCols = `run_id, idx, name, status, attempt, container_id, exit_code, output_json, log_path, log_size, started_at, finished_at, error`

func scanStep(sc interface{ Scan(...any) error }) (*RunStep, error) {
	var st RunStep
	var exit sql.NullInt64
	var started, finished sql.NullString
	err := sc.Scan(&st.RunID, &st.Idx, &st.Name, &st.Status, &st.Attempt, &st.ContainerID, &exit, &st.OutputJSON, &st.LogPath, &st.LogSize, &started, &finished, &st.Error)
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

// GetRunSteps returns the steps of a run in order.
func (s *Store) GetRunSteps(ctx context.Context, runID string) ([]*RunStep, error) {
	rows, err := s.query(ctx, `SELECT `+stepCols+` FROM run_steps WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*RunStep
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetRunStep returns one step.
func (s *Store) GetRunStep(ctx context.Context, runID string, idx int) (*RunStep, error) {
	return scanStep(s.queryRow(ctx, `SELECT `+stepCols+` FROM run_steps WHERE run_id = ? AND idx = ?`, runID, idx))
}

// UpdateRunStep writes the full mutable state of a step.
func (s *Store) UpdateRunStep(ctx context.Context, st *RunStep) error {
	var exit any
	if st.ExitCode != nil {
		exit = *st.ExitCode
	}
	if st.OutputJSON == "" {
		st.OutputJSON = "{}"
	}
	res, err := s.exec(ctx, `UPDATE run_steps SET status = ?, attempt = ?, container_id = ?, exit_code = ?, output_json = ?, log_path = ?, log_size = ?, started_at = ?, finished_at = ?, error = ?
		WHERE run_id = ? AND idx = ?`,
		st.Status, st.Attempt, st.ContainerID, exit, st.OutputJSON, st.LogPath, st.LogSize, tsPtr(st.StartedAt), tsPtr(st.FinishedAt), st.Error, st.RunID, st.Idx)
	if err != nil {
		return err
	}
	return affected(res)
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
