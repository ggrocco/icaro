package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The version history keeps goose's format, so databases migrated by
// earlier releases carry over: a version 0 row, one row per migration,
// and the latest row of a version deciding whether it is applied.
func TestMigrateKeepsGooseHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ms, err := s.migrations()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.query(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	_ = rows.Close()
	want := []int64{0}
	for _, m := range ms {
		want = append(want, m.version)
	}
	if len(got) != len(want) {
		t.Fatalf("history %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history %v, want %v", got, want)
		}
	}

	// A later row marking the last version rolled back makes it pending.
	last := ms[len(ms)-1].version
	if _, err := s.exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, ?)`, last, false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingMigrations(ctx); err != nil || n != 1 {
		t.Fatalf("%d pending, %v", n, err)
	}
}

func TestParseMigration(t *testing.T) {
	m, err := parseMigration("00003_x.sql", `-- +goose Up
-- A comment; with a semicolon.
CREATE TABLE a (
    id TEXT PRIMARY KEY
);
CREATE INDEX a_id ON a (id);
-- trailing comment

-- +goose Down
DROP TABLE a;
`)
	if err != nil {
		t.Fatal(err)
	}
	if m.version != 3 || len(m.stmts) != 2 || !strings.HasPrefix(m.stmts[0], "-- A comment; with a semicolon.\nCREATE TABLE a") || m.stmts[1] != "CREATE INDEX a_id ON a (id);" {
		t.Fatalf("%d %q", m.version, m.stmts)
	}

	for name, src := range map[string]string{
		"x_bad.sql":   "-- +goose Up\nSELECT 1;\n",
		"00001_a.sql": "-- +goose Up\nSELECT 1\n",
		"00002_a.sql": "-- +goose Up\n-- +goose StatementBegin\nSELECT 1;\n",
		"00003_a.sql": "-- +goose Down\nSELECT 1;\n",
	} {
		if _, err := parseMigration(name, src); err == nil {
			t.Errorf("%s: %q must not parse", name, src)
		}
	}
}
