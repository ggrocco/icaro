//go:build unix

package update

import (
	"context"
	"path/filepath"
	"testing"

	"icaro/internal/paths"
)

func TestRestartDaemonWithoutDaemon(t *testing.T) {
	l := paths.New(filepath.Join(t.TempDir(), "root"))
	restarted, err := RestartDaemon(context.Background(), l, "/nonexistent", nil)
	if err != nil || restarted {
		t.Fatalf("restarted=%v err=%v", restarted, err)
	}
}
