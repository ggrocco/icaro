package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenErrors covers the connection failures a misconfigured
// database.url or a broken state directory produce.
func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, SQLite, ""); err == nil {
		t.Fatal("empty sqlite path accepted")
	}
	if _, err := Open(ctx, "mysql", "x"); err == nil || !strings.Contains(err.Error(), "unsupported dialect") {
		t.Fatalf("unknown dialect: %v", err)
	}
	if _, err := Open(ctx, SQLite, filepath.Join(t.TempDir(), "missing", "t.db")); err == nil || !strings.Contains(err.Error(), "connect sqlite") {
		t.Fatalf("sqlite in a missing directory: %v", err)
	}
	// Nothing listens on port 1: the ping fails without a Postgres server.
	if _, err := Open(ctx, Postgres, "postgres://icaro@127.0.0.1:1/icaro?connect_timeout=2&sslmode=disable"); err == nil || !strings.Contains(err.Error(), "connect postgres") {
		t.Fatalf("unreachable postgres: %v", err)
	}
	if _, err := Open(ctx, Postgres, "postgres://%zz"); err == nil {
		t.Fatal("malformed postgres dsn accepted")
	}
}

func TestOpenSQLiteIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(context.Background(), SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Dialect() != SQLite || s.DB() == nil {
		t.Fatal("accessors")
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("database file mode: %v %v", fi, err)
	}
}

func TestPendingMigrations(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	n, err := s.PendingMigrations(ctx)
	if err != nil || n == 0 {
		t.Fatalf("fresh database: %d pending, %v", n, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingMigrations(ctx); err != nil || n != 0 {
		t.Fatalf("after migrate: %d pending, %v", n, err)
	}
}

// TestPostgresProvider checks that a Postgres store picks the Postgres
// migrations without needing a server: goose only reads the embedded files.
func TestPostgresProvider(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://icaro@127.0.0.1:1/icaro?sslmode=disable") // lazy: never dialed
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := (&Store{db: db, dialect: Postgres}).provider(); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Store{db: db, dialect: "nope"}).provider(); err == nil {
		t.Fatal("a dialect without migrations must fail")
	}
}

func TestParseTSAcceptsRFC3339(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := parseTS("2026-01-02T03:04:05Z"); !got.Equal(want) {
		t.Fatalf("got %v", got)
	}
	if got := parseTS("garbage"); !got.IsZero() {
		t.Fatalf("garbage parsed as %v", got)
	}
}

// TestClosedDatabase drives every repository call against a closed pool:
// each must surface the driver error instead of a zero value or a panic.
func TestClosedDatabase(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	f := MemoryFilter{ProjectID: "p"}
	calls := map[string]func() error{
		"Migrate": func() error { return s.Migrate(ctx) },
		"PendingMigrations": func() error {
			_, err := s.PendingMigrations(ctx)
			return err
		},
		"CreateDelivery": func() error { return s.CreateDelivery(ctx, &Delivery{}) },
		"FinishDelivery": func() error { return s.FinishDelivery(ctx, "d", "ok", "", "", "") },
		"GetDelivery": func() error {
			_, err := s.GetDelivery(ctx, "d")
			return err
		},
		"FindProcessedDelivery": func() error {
			_, err := s.FindProcessedDelivery(ctx, "i", "k", "w", "")
			return err
		},
		"ListDeliveries": func() error {
			_, err := s.ListDeliveries(ctx, DeliveryFilter{})
			return err
		},
		"EnsureWebhookToken": func() error {
			_, err := s.EnsureWebhookToken(ctx, "w", func() string { return "t" })
			return err
		},
		"ListWebhookTokens": func() error {
			_, err := s.ListWebhookTokens(ctx)
			return err
		},
		"RotateWebhookToken": func() error { return s.RotateWebhookToken(ctx, "w", "t") },
		"ListCronStates": func() error {
			_, err := s.ListCronStates(ctx)
			return err
		},
		"PutMemories": func() error {
			return s.PutMemories(ctx, []MemoryBundle{{Memory: &MemoryRow{ProjectID: "p"}}})
		},
		"ListMemories": func() error {
			_, err := s.ListMemories(ctx, f)
			return err
		},
		"TermPostings": func() error {
			_, _, err := s.TermPostings(ctx, "p", "t", 1)
			return err
		},
		"SearchMemoryTerms": func() error {
			_, err := s.SearchMemoryTerms(ctx, f, []string{"t"})
			return err
		},
		"FilterMemoryIDs": func() error {
			_, err := s.FilterMemoryIDs(ctx, f, []string{"x"})
			return err
		},
		"MemoryCorpusStats": func() error {
			_, _, err := s.MemoryCorpusStats(ctx, f)
			return err
		},
		"ListMemoryKeys": func() error {
			_, err := s.ListMemoryKeys(ctx, "p", "ingest")
			return err
		},
		"DeleteMemories": func() error { return s.DeleteMemories(ctx, "p", []string{"x"}) },
		"AcceptHandoff": func() error {
			_, err := s.AcceptHandoff(ctx, "p", "x", "me", time.Now())
			return err
		},
		"ListMemoryEdges": func() error {
			_, err := s.ListMemoryEdges(ctx, "p", "x")
			return err
		},
		"ExpiredMemoryIDs": func() error {
			_, err := s.ExpiredMemoryIDs(ctx, time.Now(), 0)
			return err
		},
		"CreatePreview": func() error { return s.CreatePreview(ctx, &PreviewInstance{}) },
		"UpdatePreview": func() error { return s.UpdatePreview(ctx, &PreviewInstance{ID: "x"}) },
		"ListPreviews": func() error {
			_, err := s.ListPreviews(ctx)
			return err
		},
		"PreviewAllowances": func() error {
			_, err := s.PreviewAllowances(ctx, "/w")
			return err
		},
		"CreateRun": func() error { return s.CreateRun(ctx, &Run{}, nil) },
		"EnqueueRun": func() error {
			_, err := s.EnqueueRun(ctx, &Run{Concurrency: "forbid"}, nil)
			return err
		},
		"GetRun": func() error {
			_, err := s.GetRun(ctx, "r")
			return err
		},
		"ListRuns": func() error {
			_, err := s.ListRuns(ctx, RunFilter{})
			return err
		},
		"ClaimRun": func() error {
			_, err := s.ClaimRun(ctx, "runner")
			return err
		},
		"SetRunSession": func() error { return s.SetRunSession(ctx, "r", "s") },
		"FinishRun":     func() error { return s.FinishRun(ctx, "r", RunSucceeded, "") },
		"RequestCancel": func() error {
			_, err := s.RequestCancel(ctx, "r")
			return err
		},
		"CountRunsByStatus": func() error {
			_, err := s.CountRunsByStatus(ctx)
			return err
		},
		"GetStepRuns": func() error {
			_, err := s.GetStepRuns(ctx, "r")
			return err
		},
		"UpdateStepRun": func() error { return s.UpdateStepRun(ctx, &StepRun{RunID: "r", StepID: "s"}) },
		"LastLogChunk": func() error {
			_, err := s.LastLogChunk(ctx, "r", "s", 1)
			return err
		},
		"ListLogChunks": func() error {
			_, err := s.ListLogChunks(ctx, "r", "s", 1)
			return err
		},
		"ActiveWorkflows": func() error {
			_, err := s.ActiveWorkflows(ctx)
			return err
		},
		"PutSecret": func() error { return s.PutSecret(ctx, &SecretRow{Name: "n"}) },
		"GetSecret": func() error {
			_, err := s.GetSecret(ctx, "n")
			return err
		},
		"ListSecrets": func() error {
			_, err := s.ListSecrets(ctx)
			return err
		},
		"DeleteSecret": func() error { return s.DeleteSecret(ctx, "n") },
		"UpsertProject": func() error {
			_, err := s.UpsertProject(ctx, &Project{RootPath: "/r"})
			return err
		},
		"ListProjects": func() error {
			_, err := s.ListProjects(ctx)
			return err
		},
		"UpdateSession": func() error { return s.UpdateSession(ctx, &SessionRow{ID: "s"}) },
		"ListSessions": func() error {
			_, err := s.ListSessions(ctx, SessionFilter{})
			return err
		},
		"TransitionSession": func() error {
			_, err := s.TransitionSession(ctx, "s", "running", "created")
			return err
		},
		"DeleteSession": func() error { return s.DeleteSession(ctx, "s") },
		"GetPinnedImage": func() error {
			_, err := s.GetPinnedImage(ctx, "ref")
			return err
		},
		"ListPinnedImages": func() error {
			_, err := s.ListPinnedImages(ctx)
			return err
		},
		"GetSessionRepo": func() error {
			_, err := s.GetSessionRepo(ctx, "s", "r", "v")
			return err
		},
		"ListSessionRepos": func() error {
			_, err := s.ListSessionRepos(ctx, "s")
			return err
		},
		"CreateToken": func() error {
			_, err := s.CreateToken(ctx, "n", "h", "admin")
			return err
		},
		"ListTokens": func() error {
			_, err := s.ListTokens(ctx)
			return err
		},
		"RevokeToken": func() error { return s.RevokeToken(ctx, "n") },
		"GetUISession": func() error {
			_, err := s.GetUISession(ctx, "id", UISession)
			return err
		},
		"ConsumeUILoginCode": func() error {
			_, err := s.ConsumeUILoginCode(ctx, "id")
			return err
		},
		"PurgeUISessions": func() error {
			_, err := s.PurgeUISessions(ctx)
			return err
		},
		"PutUsageSnapshots": func() error { return s.PutUsageSnapshots(ctx, []UsageSnapshot{{}}) },
		"ListUsageSnapshots": func() error {
			_, err := s.ListUsageSnapshots(ctx, "2026-01-01")
			return err
		},
		"ApplyWorkflow": func() error {
			_, err := s.ApplyWorkflow(ctx, "w", "y", "{}", "me")
			return err
		},
		"ListWorkflows": func() error {
			_, err := s.ListWorkflows(ctx)
			return err
		},
		"SetWorkflowStatus": func() error { return s.SetWorkflowStatus(ctx, "w", WorkflowDisabled) },
		"SetWebhook":        func() error { return s.SetWebhook(ctx, "w", "t", nil) },
		"DeleteWorkflow":    func() error { return s.DeleteWorkflow(ctx, "w") },
		"ListWorkflowVersions": func() error {
			_, err := s.ListWorkflowVersions(ctx, "w")
			return err
		},
		"GetWorkflowVersion": func() error {
			_, err := s.GetWorkflowVersion(ctx, "w", 1)
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s on a closed database: %v", name, err)
		}
	}
}
