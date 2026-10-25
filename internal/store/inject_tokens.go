package store

import (
	"context"
	"database/sql"
	"errors"
)

// PutInjectToken records the hash of a session's credential-injection
// capability token (ADR 0028), replacing an earlier one: each start of a
// session gets a fresh token.
func (s *Store) PutInjectToken(ctx context.Context, sessionID, tokenHash string) error {
	_, err := s.exec(ctx, `INSERT INTO session_inject_tokens (session_id, token_hash, created_at) VALUES (?,?,?)
		ON CONFLICT (session_id) DO UPDATE SET token_hash = excluded.token_hash, created_at = excluded.created_at`,
		sessionID, tokenHash, ts(Now()))
	return err
}

// InjectTokenHash returns the token hash of a session, or ErrNotFound.
func (s *Store) InjectTokenHash(ctx context.Context, sessionID string) (string, error) {
	var h string
	err := s.queryRow(ctx, `SELECT token_hash FROM session_inject_tokens WHERE session_id = ?`, sessionID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

// DeleteInjectToken forgets a session's token; a missing one is not an
// error.
func (s *Store) DeleteInjectToken(ctx context.Context, sessionID string) error {
	_, err := s.exec(ctx, `DELETE FROM session_inject_tokens WHERE session_id = ?`, sessionID)
	return err
}
