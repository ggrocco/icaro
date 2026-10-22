package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const safeCompose = `services:
  web:
    image: mirror.gcr.io/library/nginx:1.27
    ports: ["8080:80"]
`

const privilegedCompose = `services:
  web:
    image: mirror.gcr.io/library/nginx:1.27
    privileged: true
`

// Previews need Docker to start; with the engine unreachable the commands
// still list, lint, allow and report errors.
func TestPreviewCommandsWithoutDocker(t *testing.T) {
	startDaemon(t)
	safe := t.TempDir()
	writeFile(t, safe, "compose.yaml", safeCompose)
	risky := t.TempDir()
	writeFile(t, risky, "compose.yaml", privilegedCompose)

	wantCLI(t, []string{"preview", "lint", safe}, "ok: ", "passes the preview policy")
	wantCLI(t, []string{"preview", "lint", "--json", safe}, `"compose_file"`)
	out, err := runCLI(t, "", "preview", "lint", risky)
	if err == nil || !strings.Contains(err.Error(), "blocked finding(s)") || !strings.Contains(out, "[BLOCKED]") {
		t.Fatalf("lint risky: %v\n%s", err, out)
	}
	out, _ = runCLI(t, "", "preview", "lint", "--json", risky)
	var res struct {
		Blocked []struct {
			ID string `json:"id"`
		} `json:"blocked"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Blocked) == 0 {
		t.Fatalf("lint --json: %v\n%s", err, out)
	}
	failCLI(t, "", "preview", "lint", t.TempDir()) // no compose file

	failCLI(t, "name at least one --finding", "preview", "allow", risky)
	failCLI(t, "pass --yes", "preview", "allow", risky, "--finding", res.Blocked[0].ID)
	wantCLI(t, []string{"preview", "allow", risky, "--finding", res.Blocked[0].ID, "--yes"}, "[allowed]")
	wantCLI(t, []string{"preview", "allow", "--json", risky, "--finding", res.Blocked[0].ID, "--yes"}, `"findings"`)
	wantCLI(t, []string{"preview", "lint", risky}, "[allowed]")
	failCLI(t, "", "preview", "allow", risky, "--finding", "no-such-finding", "--yes")

	wantCLI(t, []string{"preview", "list"}, "ID", "STATUS")
	wantCLI(t, []string{"preview", "list", "--json"}, "")

	// Up records the preview and reports the Docker failure in its status:
	// a non-zero exit in text mode, the status field in JSON mode.
	failCLI(t, "preview is error", "preview", "up", safe)
	wantCLI(t, []string{"preview", "up", "--json", "--force", safe}, `"status": "error"`)
	// The daemon records the path with symlinks resolved (macOS temp dirs).
	resolved, _ := filepath.EvalSymlinks(safe)
	wantCLI(t, []string{"preview", "list"}, "error", resolved)
	// Logs by path find the preview; compose reports that Docker is gone.
	wantCLI(t, []string{"preview", "logs", safe}, "logs unavailable")
	_, _ = runCLI(t, "", "preview", "down", safe)
	_, _ = runCLI(t, "", "preview", "down", "--json", safe)
	_, _ = runCLI(t, "", "preview", "rm", "--yes", safe)
	failCLI(t, "", "preview", "down", t.TempDir())
	failCLI(t, "", "preview", "down", "--json", t.TempDir())
	failCLI(t, "pass --yes", "preview", "rm", safe)
	failCLI(t, "", "preview", "rm", "--yes", t.TempDir())
	failCLI(t, "no preview for", "preview", "logs", t.TempDir())
	failCLI(t, "", "preview", "logs", "prv_nope")
}

// Regression: the daemon records worktree paths with symlinks resolved, so
// `preview logs PATH` through a symlink (every macOS temp dir) must resolve
// it too instead of reporting "no preview for PATH".
func TestPreviewLogsThroughSymlink(t *testing.T) {
	startDaemon(t)
	real := t.TempDir()
	writeFile(t, real, "compose.yaml", safeCompose)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, _ = runCLI(t, "", "preview", "up", link)
	wantCLI(t, []string{"preview", "logs", link}, "logs unavailable")
}
