package store

import (
	"context"
	"database/sql"
	"errors"
)

const approvalCols = `run_id, step_id, message, decision, actor, comment, created_at, decided_at`

func scanApproval(sc interface{ Scan(...any) error }) (*Approval, error) {
	var a Approval
	var created string
	var decided sql.NullString
	err := sc.Scan(&a.RunID, &a.StepID, &a.Message, &a.Decision, &a.Actor, &a.Comment, &created, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, a.DecidedAt = parseTS(created), parseTSPtr(decided)
	return &a, nil
}

// OpenApproval records that an approval step started waiting, with the
// message shown to the approver. It keeps the row of a step that already
// started (a resumed run), so its message and any decision stand.
func (s *Store) OpenApproval(ctx context.Context, runID, stepID, message string) (*Approval, error) {
	if _, err := s.exec(ctx, `INSERT INTO step_approvals (run_id, step_id, message, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (run_id, step_id) DO NOTHING`, runID, stepID, message, ts(Now())); err != nil {
		return nil, err
	}
	return s.GetApproval(ctx, runID, stepID)
}

// GetApproval returns the approval of a run's step.
func (s *Store) GetApproval(ctx context.Context, runID, stepID string) (*Approval, error) {
	return scanApproval(s.queryRow(ctx, `SELECT `+approvalCols+` FROM step_approvals WHERE run_id = ? AND step_id = ?`, runID, stepID))
}

// ListApprovals returns a run's approvals, oldest first.
func (s *Store) ListApprovals(ctx context.Context, runID string) ([]*Approval, error) {
	rows, err := s.query(ctx, `SELECT `+approvalCols+` FROM step_approvals WHERE run_id = ? ORDER BY created_at, step_id`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval records a decision on a pending approval and wakes its
// run when it is parked, so the engine acts on it now. It returns
// ErrNotFound when the step has no approval waiting, and ErrConflict when
// it was already decided or expired.
func (s *Store) DecideApproval(ctx context.Context, runID, stepID, decision, actor, comment string) (*Approval, error) {
	now := ts(Now())
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE step_approvals SET decision = ?, actor = ?, comment = ?, decided_at = ?
			WHERE run_id = ? AND step_id = ? AND decision = ''`), decision, actor, comment, now, runID, stepID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM step_approvals WHERE run_id = ? AND step_id = ?`), runID, stepID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, s.rebind(`UPDATE runs SET wake_at = ? WHERE id = ? AND status = ?`), now, runID, RunWaiting)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetApproval(ctx, runID, stepID)
}
