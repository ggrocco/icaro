package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const connCols = `id, name, type, fields_ct, field_names, created_at, updated_at`

func scanConn(sc interface{ Scan(...any) error }) (*Connection, error) {
	var c Connection
	var names, created, updated string
	err := sc.Scan(&c.ID, &c.Name, &c.Type, &c.FieldsCT, &names, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(names), &c.FieldNames); err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = parseTS(created), parseTS(updated)
	return &c, nil
}

// UpsertConnection creates or replaces a connection by name.
func (s *Store) UpsertConnection(ctx context.Context, c *Connection) error {
	names, err := json.Marshal(c.FieldNames)
	if err != nil {
		return err
	}
	now := ts(Now())
	if c.ID == "" {
		c.ID = NewID()
	}
	_, err = s.exec(ctx, `INSERT INTO connections (`+connCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (name) DO UPDATE SET type = excluded.type, fields_ct = excluded.fields_ct, field_names = excluded.field_names, updated_at = excluded.updated_at`,
		c.ID, c.Name, c.Type, c.FieldsCT, string(names), now, now)
	return err
}

// GetConnection fetches by name.
func (s *Store) GetConnection(ctx context.Context, name string) (*Connection, error) {
	return scanConn(s.queryRow(ctx, `SELECT `+connCols+` FROM connections WHERE name = ?`, name))
}

// ListConnections returns all connections ordered by name.
func (s *Store) ListConnections(ctx context.Context) ([]*Connection, error) {
	rows, err := s.query(ctx, `SELECT `+connCols+` FROM connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Connection
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnection removes by name.
func (s *Store) DeleteConnection(ctx context.Context, name string) error {
	res, err := s.exec(ctx, `DELETE FROM connections WHERE name = ?`, name)
	if err != nil {
		return err
	}
	return affected(res)
}
