package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const awaitCols = `run_id, step_id, integration, event, workflow, match_key, state, deadline, delivery_id, payload_json, actor, created_at, decided_at`

func scanAwait(sc interface{ Scan(...any) error }) (*StepAwait, error) {
	var a StepAwait
	var deadline, created string
	var decided sql.NullString
	err := sc.Scan(&a.RunID, &a.StepID, &a.Integration, &a.Event, &a.Workflow, &a.Key, &a.State, &deadline,
		&a.DeliveryID, &a.PayloadJSON, &a.Actor, &created, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Deadline, a.CreatedAt, a.DecidedAt = parseTS(deadline), parseTS(created), parseTSPtr(decided)
	return &a, nil
}

func (s *Store) listAwaits(ctx context.Context, where string, args ...any) ([]*StepAwait, error) {
	rows, err := s.query(ctx, `SELECT `+awaitCols+` FROM step_awaits WHERE `+where+` ORDER BY created_at, run_id, step_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*StepAwait
	for rows.Next() {
		a, err := scanAwait(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OpenAwait records that an await step started waiting. It keeps the row
// of a step that already started (a resumed run), so its key, deadline and
// any resolution stand. The row is committed before the engine looks back
// through delivery_events, which is what closes the race with a delivery
// arriving meanwhile (ADR 0032).
func (s *Store) OpenAwait(ctx context.Context, a *StepAwait) (*StepAwait, error) {
	if _, err := s.exec(ctx, `INSERT INTO step_awaits (run_id, step_id, integration, event, workflow, match_key, deadline, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (run_id, step_id) DO NOTHING`,
		a.RunID, a.StepID, a.Integration, a.Event, a.Workflow, a.Key, ts(a.Deadline), ts(Now())); err != nil {
		return nil, err
	}
	return s.GetAwait(ctx, a.RunID, a.StepID)
}

// GetAwait returns the await of a run's step.
func (s *Store) GetAwait(ctx context.Context, runID, stepID string) (*StepAwait, error) {
	return scanAwait(s.queryRow(ctx, `SELECT `+awaitCols+` FROM step_awaits WHERE run_id = ? AND step_id = ?`, runID, stepID))
}

// ListAwaits returns a run's awaits, oldest first.
func (s *Store) ListAwaits(ctx context.Context, runID string) ([]*StepAwait, error) {
	return s.listAwaits(ctx, `run_id = ?`, runID)
}

// ListWaitingAwaits returns the steps still waiting for an integration's
// event, oldest first: the candidates a delivery is checked against.
func (s *Store) ListWaitingAwaits(ctx context.Context, integration, event string) ([]*StepAwait, error) {
	return s.listAwaits(ctx, `integration = ? AND event = ? AND state = ?`, integration, event, AwaitWaiting)
}

// ResolveAwait moves a waiting await to state (matched, expired or
// cancelled) with the delivery, payload and actor that resolved it. When
// it matched, the run is woken if parked, in the same transaction, so the
// engine resumes it now. It returns ErrNotFound when the step has no await
// and ErrConflict when it was already resolved.
func (s *Store) ResolveAwait(ctx context.Context, runID, stepID, state, deliveryID, payloadJSON, actor string) (*StepAwait, error) {
	now := ts(Now())
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE step_awaits SET state = ?, delivery_id = ?, payload_json = ?, actor = ?, decided_at = ?
			WHERE run_id = ? AND step_id = ? AND state = ?`), state, deliveryID, payloadJSON, actor, now, runID, stepID, AwaitWaiting)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM step_awaits WHERE run_id = ? AND step_id = ?`), runID, stepID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrConflict
		}
		if state != AwaitMatched {
			return nil
		}
		_, err = tx.ExecContext(ctx, s.rebind(`UPDATE runs SET wake_at = ? WHERE id = ? AND status = ?`), now, runID, RunWaiting)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetAwait(ctx, runID, stepID)
}

// CancelAwaits records every still-waiting await of a run as cancelled
// (run cancel, replace or workflow delete), so a later event resumes
// nothing.
func (s *Store) CancelAwaits(ctx context.Context, runID string) error {
	_, err := s.exec(ctx, `UPDATE step_awaits SET state = ?, decided_at = ? WHERE run_id = ? AND state = ?`,
		AwaitCancelled, ts(Now()), runID, AwaitWaiting)
	return err
}

// AddDeliveryEvents stores the normalized events of a verified delivery.
// Seq is the event's index in the delivery; ReceivedAt defaults to now.
func (s *Store) AddDeliveryEvents(ctx context.Context, events []DeliveryEvent) error {
	if len(events) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		q := s.rebind(`INSERT INTO delivery_events (delivery_id, seq, integration, event, workflow, payload_json, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`)
		for _, e := range events {
			if e.ReceivedAt.IsZero() {
				e.ReceivedAt = Now()
			}
			if _, err := tx.ExecContext(ctx, q, e.DeliveryID, e.Seq, e.Integration, e.Event, e.Workflow, e.PayloadJSON, ts(e.ReceivedAt)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListDeliveryEventsSince returns an integration's events of one type
// received at or after since, oldest first: the lookback of an await step
// that starts after its event may have arrived. A non-empty workflow
// keeps only events delivered on that workflow's generic webhook token.
func (s *Store) ListDeliveryEventsSince(ctx context.Context, integration, event, workflow string, since time.Time) ([]*DeliveryEvent, error) {
	q := `SELECT delivery_id, seq, integration, event, workflow, payload_json, received_at FROM delivery_events
		WHERE integration = ? AND event = ? AND received_at >= ?`
	args := []any{integration, event, ts(since)}
	if workflow != "" {
		q += ` AND workflow = ?`
		args = append(args, workflow)
	}
	rows, err := s.query(ctx, q+` ORDER BY received_at, delivery_id, seq`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*DeliveryEvent
	for rows.Next() {
		var e DeliveryEvent
		var received string
		if err := rows.Scan(&e.DeliveryID, &e.Seq, &e.Integration, &e.Event, &e.Workflow, &e.PayloadJSON, &received); err != nil {
			return nil, err
		}
		e.ReceivedAt = parseTS(received)
		out = append(out, &e)
	}
	return out, rows.Err()
}
