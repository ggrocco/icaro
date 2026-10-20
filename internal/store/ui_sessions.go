package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UI session kinds.
const (
	UISession   = "session"
	UILoginCode = "code"
)

// UISessionRow is a browser session or a one-time login code.
type UISessionRow struct {
	ID        string // SHA-256 of the secret value
	Kind      string
	Name      string
	Scope     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateUISession inserts a session or login code.
func (s *Store) CreateUISession(ctx context.Context, r *UISessionRow) error {
	r.CreatedAt = Now()
	_, err := s.exec(ctx, `INSERT INTO ui_sessions (id, kind, name, scope, created_at, expires_at) VALUES (?,?,?,?,?,?)`,
		r.ID, r.Kind, r.Name, r.Scope, ts(r.CreatedAt), ts(r.ExpiresAt))
	return err
}

// GetUISession returns an unexpired row of kind.
func (s *Store) GetUISession(ctx context.Context, id, kind string) (*UISessionRow, error) {
	var r UISessionRow
	var created, expires string
	err := s.queryRow(ctx, `SELECT id, kind, name, scope, created_at, expires_at FROM ui_sessions WHERE id = ? AND kind = ? AND expires_at > ?`,
		id, kind, ts(Now())).Scan(&r.ID, &r.Kind, &r.Name, &r.Scope, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt, r.ExpiresAt = parseTS(created), parseTS(expires)
	return &r, nil
}

// ConsumeUILoginCode deletes an unexpired login code and returns it, so a
// code works exactly once even under concurrent requests.
func (s *Store) ConsumeUILoginCode(ctx context.Context, id string) (*UISessionRow, error) {
	r, err := s.GetUISession(ctx, id, UILoginCode)
	if err != nil {
		return nil, err
	}
	res, err := s.exec(ctx, `DELETE FROM ui_sessions WHERE id = ? AND kind = ?`, id, UILoginCode)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNotFound
	}
	return r, nil
}

// DeleteUISession removes a row (logout).
func (s *Store) DeleteUISession(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM ui_sessions WHERE id = ?`, id)
	return err
}

// PurgeUISessions deletes expired rows.
func (s *Store) PurgeUISessions(ctx context.Context) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM ui_sessions WHERE expires_at <= ?`, ts(Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
