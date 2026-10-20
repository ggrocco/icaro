package chaos

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
)

// lockEnv makes the test binary act as a second process holding an
// exclusive SQLite lock: ICARO_CHAOS_LOCK=<db path>:<milliseconds>.
const lockEnv = "ICARO_CHAOS_LOCK"

func TestMain(m *testing.M) {
	if spec := os.Getenv(lockEnv); spec != "" {
		os.Exit(holdLock(spec))
	}
	os.Exit(m.Run())
}

func holdLock(spec string) int {
	i := strings.LastIndex(spec, ":")
	ms, _ := strconv.Atoi(spec[i+1:])
	db, err := sql.Open("sqlite", "file:"+spec[:i]+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 2
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(context.Background())
	if err != nil {
		return 2
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		return 3
	}
	_, _ = os.Stdout.WriteString("locked\n")
	time.Sleep(time.Duration(ms) * time.Millisecond)
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return 4
	}
	return 0
}

// lockFor starts the lock holder and returns once it holds the lock.
func lockFor(t *testing.T, dbPath string, d time.Duration) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // re-executes this test binary
	cmd.Env = append(os.Environ(), lockEnv+"="+dbPath+":"+strconv.Itoa(int(d/time.Millisecond)))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "locked\n" {
		t.Fatalf("lock holder: %q %v", buf, err)
	}
	return cmd
}

func newService(t *testing.T) (*service.Service, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "icaro.db")
	st, err := store.Open(ctx, store.SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	svc := service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := svc.ApplyWorkflow(ctx, wf("db-lock", 0), "test"); err != nil {
		t.Fatal(err)
	}
	return svc, st, path
}

// A short lock (under busy_timeout) delays writes but they succeed.
func TestShortDatabaseLockIsWaitedOut(t *testing.T) {
	svc, _, path := newService(t)
	holder := lockFor(t, path, 1500*time.Millisecond)
	start := time.Now()
	if _, err := svc.StartRun(context.Background(), "db-lock", map[string]any{}, "test"); err != nil {
		t.Fatalf("write under a short lock: %v", err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("write did not wait for the lock (%s); was it really held?", waited)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("lock holder: %v", err)
	}
}

// A lock longer than busy_timeout surfaces as an error, leaves no partial
// rows, and the database is intact and writable afterwards.
func TestLongDatabaseLockFailsCleanly(t *testing.T) {
	svc, st, path := newService(t)
	ctx := context.Background()
	holder := lockFor(t, path, 7*time.Second)
	_, err := svc.StartRun(ctx, "db-lock", map[string]any{}, "test")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "locked") && !strings.Contains(strings.ToLower(err.Error()), "busy") {
		t.Fatalf("want a locked/busy error, got %v", err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("lock holder: %v", err)
	}
	runs, err := svc.ListRuns(ctx, "db-lock", "", 10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("a failed write left %d run(s): %v", len(runs), err)
	}
	var ok string
	if err := st.DB().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check = %q %v", ok, err)
	}
	if _, err := svc.StartRun(ctx, "db-lock", map[string]any{}, "test"); err != nil {
		t.Fatalf("write after the lock was released: %v", err)
	}
}
