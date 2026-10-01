package store

import (
	"context"
	"database/sql"
	"errors"
)

// GetMeta reads a runner_meta value.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.queryRow(ctx, `SELECT value FROM runner_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetMeta writes a runner_meta value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.exec(ctx, `INSERT INTO runner_meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
