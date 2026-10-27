package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunDockerCLIUsesDockerHost: the docker CLI that sh/claude/login run
// targets docker.host when it is configured, like the daemon does.
func TestRunDockerCLIUsesDockerHost(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env")
	script := "#!/bin/sh\necho \"$DOCKER_HOST\" > " + out + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///from/env.sock")

	err := runDockerCLI(context.Background(), "tcp://engine:2376", []string{"version"})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("exit code: %v", err)
	}
	if b, _ := os.ReadFile(out); strings.TrimSpace(string(b)) != "tcp://engine:2376" { //nolint:gosec // test file
		t.Fatalf("DOCKER_HOST = %q, want docker.host", b)
	}
	// Without docker.host the user's environment is kept.
	_ = runDockerCLI(context.Background(), "", []string{"version"})
	if b, _ := os.ReadFile(out); strings.TrimSpace(string(b)) != "unix:///from/env.sock" { //nolint:gosec // test file
		t.Fatalf("DOCKER_HOST = %q, want the environment's", b)
	}
}
