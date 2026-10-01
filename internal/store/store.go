// Package store persists engine state behind a small repository API with
// portable SQL that runs unchanged on SQLite and Postgres.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver "pgx"
	"github.com/oklog/ulid/v2"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // sqlite driver "sqlite"
)

//go:embed migrations
var migrationsFS embed.FS

// Dialect identifies the SQL backend.
type Dialect string

// Supported dialects.
const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a unique constraint is violated.
var ErrConflict = errors.New("already exists")

// Store is the repository root.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

// Open connects to the database. For sqlite, dsn is the file path; WAL,
// busy_timeout and foreign keys are enabled and the pool is limited to one
// connection because SQLite is single-writer.
func Open(ctx context.Context, dialect Dialect, dsn string) (*Store, error) {
	var db *sql.DB
	var err error
	switch dialect {
	case SQLite:
		if dsn == "" {
			return nil, errors.New("sqlite path is required")
		}
		db, err = sql.Open("sqlite", "file:"+dsn+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
		if err == nil {
			db.SetMaxOpenConns(1)
		}
	case Postgres:
		db, err = sql.Open("pgx", dsn)
		if err == nil {
			db.SetMaxOpenConns(10)
		}
	default:
		return nil, fmt.Errorf("unsupported dialect %q", dialect)
	}
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect %s: %w", dialect, err)
	}
	if dialect == SQLite {
		// The driver creates the file with the process umask; state is private.
		if err := os.Chmod(dsn, 0o600); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Store{db: db, dialect: dialect}, nil
}

// Dialect returns the backend in use.
func (s *Store) Dialect() Dialect { return s.dialect }

// DB exposes the underlying pool (tests and migrations).
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// Migrate applies pending migrations for the dialect.
func (s *Store) Migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrationsFS, "migrations/"+string(s.dialect))
	if err != nil {
		return err
	}
	gd := goose.DialectSQLite3
	if s.dialect == Postgres {
		gd = goose.DialectPostgres
	}
	p, err := goose.NewProvider(gd, s.db, sub)
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

// rebind rewrites ? placeholders to $N for postgres.
func (s *Store) rebind(q string) string {
	if s.dialect != Postgres {
		return q
	}
	var sb strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			sb.WriteByte('$')
			sb.WriteString(strconv.Itoa(n))
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(q), args...)
}

// tx runs fn in a transaction.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// NewID returns a sortable unique id.
func NewID() string { return ulid.Make().String() }

// Timestamps are stored as fixed-width UTC strings so they sort correctly
// as text on both backends.
const tsLayout = "2006-01-02T15:04:05.000000Z07:00"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func tsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func parseTS(s string) time.Time {
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	return t
}

func parseTSPtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTS(s.String)
	return &t
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || // sqlite
		strings.Contains(msg, "SQLSTATE 23505") // postgres
}

// Now is the clock used for timestamps; overridable in tests.
var Now = time.Now
