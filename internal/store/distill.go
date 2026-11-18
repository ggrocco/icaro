package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Distill job statuses.
const (
	DistillPending = "pending"
	DistillRunning = "running"
	DistillDone    = "done"
	DistillFailed  = "failed"
)

// DistillJob is one chat's place in the memory distiller's queue.
type DistillJob struct {
	ChatID    string
	Status    string
	Attempts  int
	Error     string
	Stage2    string
	Memories  int
	RunAt     time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

const distillCols = `chat_id, status, attempts, error, stage2, memories, run_at, created_at, updated_at`

func scanDistill(sc scanner) (*DistillJob, error) {
	var j DistillJob
	var runAt, created, updated string
	err := sc.Scan(&j.ChatID, &j.Status, &j.Attempts, &j.Error, &j.Stage2, &j.Memories, &runAt, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.RunAt, j.CreatedAt, j.UpdatedAt = parseTS(runAt), parseTS(created), parseTS(updated)
	return &j, nil
}

// EnqueueDistill queues a chat to be distilled at runAt. A chat already
// queued keeps its place; a finished one is queued again with a fresh
// attempt count. It reports whether the chat was queued by this call.
func (s *Store) EnqueueDistill(ctx context.Context, chatID string, runAt time.Time) (bool, error) {
	now := ts(Now())
	queued := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE distill_jobs SET status = ?, attempts = 0, error = '', run_at = ?, updated_at = ?
			WHERE chat_id = ? AND status IN (?, ?)`), DistillPending, ts(runAt), now, chatID, DistillDone, DistillFailed)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			queued = true
			return nil
		}
		res, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO distill_jobs (`+distillCols+`) VALUES (?,?,0,'','',0,?,?,?)
			ON CONFLICT (chat_id) DO NOTHING`), chatID, DistillPending, ts(runAt), now, now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		queued = n > 0
		return nil
	})
	return queued, err
}

// ClaimDistill claims the oldest due pending job (pending -> running).
// It returns ErrNotFound when none is due.
func (s *Store) ClaimDistill(ctx context.Context, now time.Time) (*DistillJob, error) {
	for range 5 {
		j, err := scanDistill(s.queryRow(ctx, `SELECT `+distillCols+` FROM distill_jobs WHERE status = ? AND run_at <= ? ORDER BY run_at, chat_id LIMIT 1`,
			DistillPending, ts(now)))
		if err != nil {
			return nil, err
		}
		res, err := s.exec(ctx, `UPDATE distill_jobs SET status = ?, attempts = attempts + 1, updated_at = ? WHERE chat_id = ? AND status = ?`,
			DistillRunning, ts(Now()), j.ChatID, DistillPending)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			j.Status, j.Attempts = DistillRunning, j.Attempts+1
			return j, nil
		}
	}
	return nil, ErrNotFound
}

// FinishDistill records a running job's outcome: done, failed, or pending
// again at retryAt for another attempt.
func (s *Store) FinishDistill(ctx context.Context, j *DistillJob) error {
	res, err := s.exec(ctx, `UPDATE distill_jobs SET status = ?, error = ?, stage2 = ?, memories = ?, run_at = ?, updated_at = ?
		WHERE chat_id = ? AND status = ?`, j.Status, j.Error, j.Stage2, j.Memories, ts(j.RunAt), ts(Now()), j.ChatID, DistillRunning)
	return wrote(res, err)
}

// ResetDistills puts jobs a restart interrupted back in the queue.
func (s *Store) ResetDistills(ctx context.Context) (int, error) {
	res, err := s.exec(ctx, `UPDATE distill_jobs SET status = ?, updated_at = ? WHERE status = ?`, DistillPending, ts(Now()), DistillRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetDistill returns a chat's job.
func (s *Store) GetDistill(ctx context.Context, chatID string) (*DistillJob, error) {
	return scanDistill(s.queryRow(ctx, `SELECT `+distillCols+` FROM distill_jobs WHERE chat_id = ?`, chatID))
}

// LastDistill returns the most recently finished job, or ErrNotFound.
func (s *Store) LastDistill(ctx context.Context) (*DistillJob, error) {
	return scanDistill(s.queryRow(ctx, `SELECT `+distillCols+` FROM distill_jobs WHERE status IN (?, ?) ORDER BY updated_at DESC, chat_id LIMIT 1`,
		DistillDone, DistillFailed))
}

// DeleteDistill drops a chat's job and stats (the chat was deleted).
func (s *Store) DeleteDistill(ctx context.Context, chatID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM distill_jobs WHERE chat_id = ?`, `DELETE FROM chat_stats WHERE chat_id = ?`, `DELETE FROM distill_removed WHERE chat_id = ?`} {
			if _, err := tx.ExecContext(ctx, s.rebind(q), chatID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ChatStats are one chat's convergence metrics (ADR 0036); -1 means the
// event never happened.
type ChatStats struct {
	ChatID        string
	UsedMemories  int
	Turns         int
	Validations   int
	Proposals     int
	Saves         int
	TurnsToSave   int
	SecondsToSave int
	UpdatedAt     time.Time
}

const chatStatsCols = `chat_id, used_memories, turns, validations, proposals, saves, turns_to_save, seconds_to_save, updated_at`

// PutChatStats replaces a chat's metrics.
func (s *Store) PutChatStats(ctx context.Context, st *ChatStats) error {
	st.UpdatedAt = Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM chat_stats WHERE chat_id = ?`), st.ChatID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO chat_stats (`+chatStatsCols+`) VALUES (?,?,?,?,?,?,?,?,?)`),
			st.ChatID, st.UsedMemories, st.Turns, st.Validations, st.Proposals, st.Saves, st.TurnsToSave, st.SecondsToSave, ts(st.UpdatedAt))
		return err
	})
}

// ListChatStats returns every chat's metrics.
func (s *Store) ListChatStats(ctx context.Context) ([]*ChatStats, error) {
	rows, err := s.query(ctx, `SELECT `+chatStatsCols+` FROM chat_stats ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ChatStats
	for rows.Next() {
		var c ChatStats
		var updated string
		if err := rows.Scan(&c.ChatID, &c.UsedMemories, &c.Turns, &c.Validations, &c.Proposals, &c.Saves, &c.TurnsToSave, &c.SecondsToSave, &updated); err != nil {
			return nil, err
		}
		c.UpdatedAt = parseTS(updated)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// MarkDistillRemoved records that a person removed a chat's memory.
func (s *Store) MarkDistillRemoved(ctx context.Context, chatID, memoryID string) error {
	_, err := s.exec(ctx, `INSERT INTO distill_removed (chat_id, memory_id, created_at) VALUES (?,?,?) ON CONFLICT (chat_id, memory_id) DO NOTHING`,
		chatID, memoryID, ts(Now()))
	return err
}

// DistillRemoved lists the memories a person removed from a chat.
func (s *Store) DistillRemoved(ctx context.Context, chatID string) (map[string]bool, error) {
	rows, err := s.query(ctx, `SELECT memory_id FROM distill_removed WHERE chat_id = ?`, chatID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
