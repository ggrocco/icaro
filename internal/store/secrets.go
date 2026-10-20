package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SecretRow is a sealed named secret.
type SecretRow struct {
	Name      string
	ValueCT   []byte
	Hosts     []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PutSecret inserts or replaces a secret.
func (s *Store) PutSecret(ctx context.Context, r *SecretRow) error {
	hosts, err := json.Marshal(orEmpty(r.Hosts))
	if err != nil {
		return err
	}
	now := ts(Now())
	_, err = s.exec(ctx, `INSERT INTO secrets (name, value_ct, hosts_json, created_at, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT (name) DO UPDATE SET value_ct = excluded.value_ct, hosts_json = excluded.hosts_json, updated_at = excluded.updated_at`,
		r.Name, r.ValueCT, string(hosts), now, now)
	return err
}

// GetSecret fetches one secret.
func (s *Store) GetSecret(ctx context.Context, name string) (*SecretRow, error) {
	var r SecretRow
	var hosts, created, updated string
	err := s.queryRow(ctx, `SELECT name, value_ct, hosts_json, created_at, updated_at FROM secrets WHERE name = ?`, name).
		Scan(&r.Name, &r.ValueCT, &hosts, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(hosts), &r.Hosts)
	r.CreatedAt, r.UpdatedAt = parseTS(created), parseTS(updated)
	return &r, nil
}

// ListSecrets returns secrets ordered by name (values included, sealed).
func (s *Store) ListSecrets(ctx context.Context) ([]*SecretRow, error) {
	rows, err := s.query(ctx, `SELECT name, value_ct, hosts_json, created_at, updated_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*SecretRow
	for rows.Next() {
		var r SecretRow
		var hosts, created, updated string
		if err := rows.Scan(&r.Name, &r.ValueCT, &hosts, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(hosts), &r.Hosts)
		r.CreatedAt, r.UpdatedAt = parseTS(created), parseTS(updated)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteSecret removes a secret.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	res, err := s.exec(ctx, `DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	return affected(res)
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
