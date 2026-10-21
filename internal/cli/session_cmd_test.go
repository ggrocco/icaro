package cli

import (
	"strings"
	"testing"
)

// Creating a session waits for Docker (the integration and e2e suites
// cover it); with the engine unreachable the read and error paths of the
// session commands still run against the daemon.
func TestSessionCommandsWithoutDocker(t *testing.T) {
	startDaemon(t)

	wantCLI(t, []string{"session", "list"}, "ID", "KIND", "WORKTREE")
	wantCLI(t, []string{"session", "list", "--json", "--kind", "agent", "--status", "running"}, "[]")
	failCLI(t, "unknown project", "session", "list", "--project", "x")
	wantCLI(t, []string{"session", "projects"}, "NAME", "ROOT")
	failCLI(t, "", "session", "get", "ses_nope")
	failCLI(t, "", "session", "get", "--json", "ses_nope")
	failCLI(t, "", "session", "stop", "ses_nope")
	failCLI(t, "", "session", "start", "ses_nope")
	failCLI(t, "", "session", "attach", "ses_nope")
	failCLI(t, "", "sh", "ses_nope", "--", "true")

	// rm asks first; a "no" changes nothing and contacts no one.
	if out, err := runCLI(t, "n\n", "session", "rm", "ses_nope", "--keep-worktree"); err != nil || !strings.Contains(out, "(keeping its worktree)") {
		t.Fatalf("rm declined: %v\n%s", err, out)
	}
	if out, err := runCLI(t, "y\n", "session", "rm", "ses_nope"); err == nil || !strings.Contains(out, "Uncommitted changes are lost") {
		t.Fatalf("rm of an unknown session: %v\n%s", err, out)
	}
	failCLI(t, "", "session", "rm", "ses_nope", "--yes")
}
