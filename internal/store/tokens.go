package store

import (
	"context"
	"database/sql"
	"errors"
)

const tokenCols = `id, name, token_hash, scope, created_at, last_used_at, revoked_at` //nolint:gosec // column list, not a credential

func scanToken(sc interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var created string
	var used, revoked sql.NullString
	err := sc.Scan(&t.ID, &t.Name, &t.TokenHash, &t.Scope, &created, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = parseTS(created)
	t.LastUsedAt, t.RevokedAt = parseTSPtr(used), parseTSPtr(revoked)
	return &t, nil
}

// CreateToken stores a token hash. Returns ErrConflict on duplicate name.
func (s *Store) CreateToken(ctx context.Context, name, hash, scope string) (*APIToken, error) {
	t := &APIToken{ID: NewID(), Name: name, TokenHash: hash, Scope: scope, CreatedAt: Now()}
	_, err := s.exec(ctx, `INSERT INTO api_tokens (`+tokenCols+`) VALUES (?,?,?,?,?,?,?)`, t.ID, t.Name, t.TokenHash, t.Scope, ts(t.CreatedAt), nil, nil)
	if isUniqueViolation(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// GetTokenByHash looks up an unrevoked token by hash.
func (s *Store) GetTokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	return scanToken(s.queryRow(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE token_hash = ? AND revoked_at IS NULL`, hash))
}

// TouchToken records last use.
func (s *Store) TouchToken(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ts(Now()), id)
	return err
}

// ListTokens returns all tokens (hashes included; callers must not expose them).
func (s *Store) ListTokens(ctx context.Context) ([]*APIToken, error) {
	rows, err := s.query(ctx, `SELECT `+tokenCols+` FROM api_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes by name.
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.exec(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`, ts(Now()), name)
	if err != nil {
		return err
	}
	return affected(res)
}
