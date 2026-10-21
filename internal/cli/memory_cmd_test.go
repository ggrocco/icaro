package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// projectRepo is a small git repository for memory commands to scope to.
func projectRepo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "--quiet")
	writeFile(t, repo, "main.go", "package main\n\n// Hello greets.\nfunc Hello() string { return \"hi\" }\n\nfunc main() {}\n")
	if err := os.MkdirAll(filepath.Join(repo, "docs", "adr"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "docs", "adr"), "0001-use-go.md", "# ADR 0001 — Use Go\n\n**Status:** Accepted\n\nWe use Go.\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "initial commit")
	return repo
}

func TestMemoryCommands(t *testing.T) {
	startDaemon(t)
	repo := projectRepo(t)
	p := []string{"memory", "--project", repo}
	cmd := func(args ...string) []string { return append(append([]string{}, p...), args...) }

	out := mustCLI(t, cmd("project")...)
	pid := strings.Fields(out)[0]
	if !strings.Contains(out, repo) || pid == "" {
		t.Fatalf("project: %s", out)
	}
	wantCLI(t, []string{"memory", "project", "--json", repo}, `"project": "`+pid+`"`)

	out = wantCLI(t, cmd("write", "--kind", "decision", "--title", "Use SQLite", "--body", "embedded and simple", "--source", "main.go", "--agent", "claude"), "decision", `"Use SQLite"`)
	decision := strings.Fields(out)[0]
	bodyFile := writeFile(t, t.TempDir(), "body.md", "from a file")
	var m struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(mustCLI(t, cmd("write", "--json", "--kind", "finding", "--title", "Flaky test", "--body-file", bodyFile, "--supersedes", "")...)), &m); err != nil || m.Body != "from a file" {
		t.Fatalf("write --json: %v %+v", err, m)
	}
	failCLI(t, "no such file", cmd("write", "--kind", "finding", "--title", "x", "--body-file", "/nonexistent/body")...)
	failCLI(t, "", cmd("write", "--kind", "nonsense", "--title", "x")...)

	wantCLI(t, cmd("query", "SQLite"), "ID", decision, "Use SQLite")
	wantCLI(t, cmd("query", "--kind", "finding", "--since", "1h", "--limit", "5"), "Flaky test")
	wantCLI(t, cmd("query", "--since", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "--agent", "claude", "--source", "main"), "Use SQLite")
	wantCLI(t, cmd("query", "--json"), `"results"`)
	failCLI(t, "--since must be", cmd("query", "--since", "yesterday")...)

	wantCLI(t, cmd("show", decision), decision, "Use SQLite", "embedded and simple", "source:  main.go", "origin:  cli", "scope:   branch=")
	wantCLI(t, cmd("show", "--json", decision), `"id": "`+decision+`"`)
	failCLI(t, "", cmd("show", "mem_nope")...)

	// Handoffs: pending until accepted.
	out = mustCLI(t, cmd("write", "--kind", "handoff", "--title", "Finish the API", "--body", "tests left")...)
	handoff := strings.Fields(out)[0]
	wantCLI(t, cmd("handoff", "list"), handoff, "Finish the API")
	wantCLI(t, cmd("handoff", "list", "--json", "--branch", "main"), handoff)
	wantCLI(t, cmd("handoff", "accept", handoff, "--agent", "codex"), "accepted "+handoff, "tests left")
	wantCLI(t, cmd("show", handoff), "status:  accepted codex")
	failCLI(t, "", cmd("handoff", "accept", handoff)...)
	if err := json.Unmarshal([]byte(mustCLI(t, cmd("handoff", "list", "--json")...)), &struct{}{}); err != nil {
		t.Fatal(err)
	}
	out = mustCLI(t, cmd("write", "--kind", "handoff", "--title", "Second")...)
	wantCLI(t, cmd("handoff", "accept", "--json", strings.Fields(out)[0]), `"title": "Second"`)

	// Ingest indexes files, symbols, ADRs and commits; a second run only
	// reports unchanged files.
	wantCLI(t, cmd("ingest"), pid+":", "files", "1 ADRs", "1 commits indexed")
	wantCLI(t, []string{"memory", "ingest", "--json", "--commits", "-1", repo}, `"unchanged"`)
	wantCLI(t, cmd("query", "Hello"), "Hello")
	failCLI(t, "", "memory", "ingest", filepath.Join(repo, "missing"))

	// Import adapters.
	graphify := filepath.Join("..", "memory", "testdata", "graphify.json")
	wantCLI(t, cmd("import", "--format", "graphify", graphify), "imported ")
	wantCLI(t, cmd("import", "--json", "--format", "graphify", graphify), `"unchanged"`)
	failCLI(t, "", cmd("import", "--format", "nope", graphify)...)
	failCLI(t, "no such file", cmd("import", "--format", "graphify", "/nonexistent.json")...)

	// --project-id overrides the path.
	wantCLI(t, []string{"memory", "--project-id", pid, "query", "SQLite"}, decision)
}

func TestDoctorReportsAndPurgesExpiredMemories(t *testing.T) {
	startDaemon(t)
	repo := projectRepo(t)
	mustCLI(t, "memory", "--project", repo, "write", "--kind", "task", "--title", "short-lived", "--ttl", "1s")
	time.Sleep(1100 * time.Millisecond)

	// Docker is unreachable, so doctor reports errors overall; the memory
	// check still runs and warns about the expired memory.
	out, err := runCLI(t, "", "doctor")
	if err == nil || !strings.Contains(err.Error(), "doctor found errors") {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 expired memories not yet purged") {
		t.Fatalf("doctor output:\n%s", out)
	}
	out, _ = runCLI(t, "", "doctor", "--fix", "--json")
	if !strings.Contains(out, "purged 1") {
		t.Fatalf("doctor --fix:\n%s", out)
	}
	out, _ = runCLI(t, "", "doctor")
	if !strings.Contains(out, "no expired memories pending purge") {
		t.Fatalf("doctor after fix:\n%s", out)
	}
}
