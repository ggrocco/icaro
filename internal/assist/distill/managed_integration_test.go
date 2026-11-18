//go:build integration

package distill

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
)

// TestManagedRuntimeWithARealModel starts llama-server the way the
// daemon does and summarizes a short chat. It needs Docker and a pulled
// model: ICARO_TEST_DISTILL_MODELS names a directory holding the
// smallest catalog model (icaro memory model pull qwen3.5-0.8b).
func TestManagedRuntimeWithARealModel(t *testing.T) {
	dir := os.Getenv("ICARO_TEST_DISTILL_MODELS")
	if dir == "" {
		t.Skip("ICARO_TEST_DISTILL_MODELS is not set")
	}
	eng, err := docker.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := eng.Ping(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	m, err := Lookup("qwen3.5-0.8b")
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: dir}
	if err := store.Verify(m); err != nil {
		t.Fatal(err)
	}
	sock, err := os.MkdirTemp("/tmp", "icit-distill-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(sock) }()
	key := fmt.Sprintf("it%d", time.Now().UnixNano())
	rt := Managed{Docker: eng, Models: store, SocketDir: sock, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), CPUs: 2, Name: "icaro-it-distill-" + key, Key: key}
	c, stop, err := rt.Start(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	s, u, err := Summarize(ctx, c, "bug-to-slack", []Turn{
		{Role: "user", Text: "When a GitHub issue gets the bug label, post it to the #bugs channel in Slack."},
		{Role: "assistant", Text: "I proposed bug-to-slack with a github.issues trigger and a slack.post_message step to #bugs. It validated."},
	})
	if err != nil || u.Calls != 1 {
		t.Fatalf("%v %+v", err, u)
	}
	if s.Intent == "" || !strings.Contains(strings.ToLower(s.Intent+s.Resume+strings.Join(s.Constraints, " ")), "bug") {
		t.Fatalf("%+v", s)
	}
	stop()
	if ids, _ := eng.ListByLabel(ctx, map[string]string{"icaro.distill": key}); len(ids) != 0 {
		t.Fatalf("container left behind: %v", ids)
	}
}
