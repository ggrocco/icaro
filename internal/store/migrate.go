package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// Migrations are numbered SQL files (NNNNN_name.sql) embedded per dialect,
// each with a "-- +goose Up" section and a "-- +goose Down" section that
// Ícaro never runs. Applied versions are recorded in goose_db_version, the
// table and format of the goose library earlier releases migrated with, so
// their databases carry over unchanged.

const versionTable = "goose_db_version"

type migration struct {
	version int64
	name    string
	stmts   []string // the Up section, one statement each
}

// migrations reads the embedded migrations of the store's dialect, in
// version order.
func (s *Store) migrations() ([]migration, error) {
	dir := "migrations/" + string(s.dialect)
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil || len(entries) == 0 {
		return nil, fmt.Errorf("no migrations for dialect %q", s.dialect)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := fs.ReadFile(migrationsFS, dir+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		m, err := parseMigration(e.Name(), string(data))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("migrations %s and %s share version %d", out[i-1].name, out[i].name, out[i].version)
		}
	}
	return out, nil
}

// parseMigration splits the Up section into statements. A statement ends
// at a line ending in a semicolon; comment lines never end one.
func parseMigration(name, src string) (migration, error) {
	num, _, _ := strings.Cut(name, "_")
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil || v < 1 {
		return migration{}, fmt.Errorf("migration %s: the name must start with a version number", name)
	}
	m := migration{version: v, name: name}
	var cur strings.Builder
	up, seenUp := false, false
	for line := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(line)
		if a, ok := strings.CutPrefix(t, "-- +goose "); ok {
			switch a {
			case "Up":
				up, seenUp = true, true
			case "Down":
				up = false
			default:
				return migration{}, fmt.Errorf("migration %s: unsupported annotation %q", name, t)
			}
			continue
		}
		if !up {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if !strings.HasPrefix(t, "--") && strings.HasSuffix(t, ";") {
			m.stmts = append(m.stmts, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	for line := range strings.SplitSeq(cur.String(), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
			return migration{}, fmt.Errorf("migration %s: a statement does not end with a semicolon", name)
		}
	}
	if !seenUp || len(m.stmts) == 0 {
		return migration{}, fmt.Errorf("migration %s: no Up statements", name)
	}
	return m, nil
}

// Migrate applies pending migrations for the dialect, each in its own
// transaction with its version row.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := s.migrations()
	if err != nil {
		return err
	}
	exists, err := s.versionTableExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		if err := s.createVersionTable(ctx); err != nil {
			return err
		}
	}
	done, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if done[m.version] {
			continue
		}
		err := s.tx(ctx, func(tx *sql.Tx) error {
			for _, q := range m.stmts {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			return s.recordVersion(ctx, tx, m.version)
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// PendingMigrations returns how many migrations have not been applied. It
// only reads: a database never migrated has every migration pending.
func (s *Store) PendingMigrations(ctx context.Context) (int, error) {
	ms, err := s.migrations()
	if err != nil {
		return 0, err
	}
	exists, err := s.versionTableExists(ctx)
	if err != nil || !exists {
		return len(ms), err
	}
	done, err := s.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range ms {
		if !done[m.version] {
			n++
		}
	}
	return n, nil
}

func (s *Store) versionTableExists(ctx context.Context) (bool, error) {
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`
	if s.dialect == Postgres {
		q = `SELECT COUNT(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename = ?`
	}
	var n int
	if err := s.queryRow(ctx, q, versionTable).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// createVersionTable creates goose's table with its initial version 0 row.
func (s *Store) createVersionTable(ctx context.Context) error {
	ddl := `CREATE TABLE ` + versionTable + ` (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now'))
	)`
	if s.dialect == Postgres {
		ddl = `CREATE TABLE ` + versionTable + ` (
		id integer PRIMARY KEY GENERATED BY DEFAULT AS IDENTITY,
		version_id bigint NOT NULL,
		is_applied boolean NOT NULL,
		tstamp timestamp NOT NULL DEFAULT now()
	)`
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return err
		}
		return s.recordVersion(ctx, tx, 0)
	})
}

func (s *Store) recordVersion(ctx context.Context, tx *sql.Tx, v int64) error {
	_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO `+versionTable+` (version_id, is_applied) VALUES (?, ?)`), v, true)
	return err
}

// appliedVersions reads the version history; the latest row of a version
// says whether it is applied (goose records a rollback as a new row).
func (s *Store) appliedVersions(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.query(ctx, `SELECT version_id, is_applied FROM `+versionTable+` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[int64]bool{}
	done := map[int64]bool{}
	for rows.Next() {
		var v int64
		var applied bool
		if err := rows.Scan(&v, &applied); err != nil {
			return nil, err
		}
		if !seen[v] {
			seen[v] = true
			done[v] = applied
		}
	}
	return done, rows.Err()
}
