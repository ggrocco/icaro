package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RemoteHostRow is a remote daemon this daemon is signed in to (ADR 0034).
type RemoteHostRow struct {
	Alias     string
	Label     string
	URL       string
	TokenCT   []byte // the API token, sealed with the master key
	Principal string
	Scope     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const remoteHostCols = `alias, label, url, token_ct, principal, scope, created_at, updated_at`

// PutRemoteHost signs in to a host, or replaces the sign-in of an alias.
func (s *Store) PutRemoteHost(ctx context.Context, r *RemoteHostRow) error {
	now := ts(Now())
	_, err := s.exec(ctx, `INSERT INTO remote_hosts (`+remoteHostCols+`) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (alias) DO UPDATE SET label = excluded.label, url = excluded.url, token_ct = excluded.token_ct,
		principal = excluded.principal, scope = excluded.scope, updated_at = excluded.updated_at`,
		r.Alias, r.Label, r.URL, r.TokenCT, r.Principal, r.Scope, now, now)
	return err
}

func scanRemoteHost(sc interface{ Scan(...any) error }) (*RemoteHostRow, error) {
	var r RemoteHostRow
	var created, updated string
	if err := sc.Scan(&r.Alias, &r.Label, &r.URL, &r.TokenCT, &r.Principal, &r.Scope, &created, &updated); err != nil {
		return nil, err
	}
	r.CreatedAt, r.UpdatedAt = parseTS(created), parseTS(updated)
	return &r, nil
}

// GetRemoteHost fetches one host by alias.
func (s *Store) GetRemoteHost(ctx context.Context, alias string) (*RemoteHostRow, error) {
	r, err := scanRemoteHost(s.queryRow(ctx, `SELECT `+remoteHostCols+` FROM remote_hosts WHERE alias = ?`, alias))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListRemoteHosts returns every host ordered by alias.
func (s *Store) ListRemoteHosts(ctx context.Context) ([]*RemoteHostRow, error) {
	rows, err := s.query(ctx, `SELECT `+remoteHostCols+` FROM remote_hosts ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*RemoteHostRow
	for rows.Next() {
		r, err := scanRemoteHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRemoteHost signs out of a host.
func (s *Store) DeleteRemoteHost(ctx context.Context, alias string) error {
	res, err := s.exec(ctx, `DELETE FROM remote_hosts WHERE alias = ?`, alias)
	if err != nil {
		return err
	}
	return affected(res)
}
