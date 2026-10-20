package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/config"
	"icaro/internal/crypto"
	"icaro/internal/doctor"
	"icaro/internal/secrets"
	"icaro/internal/store"
)

func TestDoctorChecks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wf := "version: 1\nname: review\ntriggers:\n  - integration: github\n    event: pull_request.opened\nsteps:\n  - id: a\n    image: alpine\n"
	if _, err := db.ApplyWorkflow(ctx, "review", wf, "{}", "t"); err != nil {
		t.Fatal(err)
	}
	reps := doctor.Run(ctx, DoctorChecks(dir, db), false)
	if reps[0].Severity != doctor.OK || reps[1].Severity != doctor.Error || !strings.Contains(reps[1].Detail, "icaro credential add github webhook_secret") {
		t.Fatalf("%+v", reps)
	}
	key, _ := crypto.NewKey()
	if err := secrets.New(db, key).Set(ctx, "github.webhook_secret", "abcdef123", nil); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "bad"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "bad", "x.yaml"), []byte("version: 2\n"), 0o600)
	reps = doctor.Run(ctx, DoctorChecks(dir, db), false)
	if reps[0].Severity != doctor.Error || !strings.Contains(reps[0].Detail, "bad/x.yaml") || reps[1].Severity != doctor.OK {
		t.Fatalf("%+v", reps)
	}
}

func TestPlanCredential(t *testing.T) {
	m, _, _ := Parse(builtinFiles()["jira.yaml"])
	if _, err := PlanCredential(m, "token", config.IntegrationConfig{}); err != nil {
		t.Fatal(err)
	}
	p, _ := PlanCredential(m, "token", config.IntegrationConfig{BaseURL: "https://jira.corp.example"})
	if strings.Join(p.Hosts, ",") != "*.atlassian.net,jira.corp.example" {
		t.Fatal(p.Hosts)
	}
	p, _ = PlanCredential(m, "webhook_secret", config.IntegrationConfig{})
	if !p.Webhook || len(p.Hosts) != 0 {
		t.Fatal(p)
	}
	if _, err := PlanCredential(m, "nope", config.IntegrationConfig{}); err == nil || !strings.Contains(err.Error(), "token, webhook_secret") {
		t.Fatal(err)
	}
}
