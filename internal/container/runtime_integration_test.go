//go:build integration

package container

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/sandbox"
	"icaro/internal/doctor"
)

// TestEngineRuntimes: the engine lists its registered runtimes, the one
// these tests run on among them, and the doctor check accepts a profile
// on it.
func TestEngineRuntimes(t *testing.T) {
	eng := engine(t)
	ctx := context.Background()
	rts, err := eng.Runtimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "runc"
	if testRuntime != "" {
		want = testRuntime
	}
	if !slices.Contains(rts, want) {
		t.Fatalf("registered runtimes %v lack %q", rts, want)
	}
	if r := RuntimeCheck(eng, map[string]string{"p": want}).Run(ctx); r.Severity != doctor.OK {
		t.Fatalf("%+v", r)
	}
	if r := RuntimeCheck(eng, map[string]string{"p": "no-such-runtime"}).Run(ctx); r.Severity != doctor.Error {
		t.Fatalf("%+v", r)
	}
}

// TestAgentReachesTheSessionSocket: the agent's MCP bridge (`icaro mcp
// --socket /icaro/mcp.sock`, the command agent configs run) reaches the
// daemon's per-session socket from inside the container. Under gVisor a
// sandbox may connect to a host Unix socket only when runsc runs with
// --host-uds=open, which docs/security.md registers it with.
func TestAgentReachesTheSessionSocket(t *testing.T) {
	eng := engine(t)
	ctx := context.Background()
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	// A short directory: a socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("/tmp", "icit-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = os.Chmod(dir, 0o777)
	sock := filepath.Join(dir, "mcp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(sock, 0o777)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"reached":"host"}}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	bin := staticIcaro(t)
	netName := "icaro-it-sock-" + time.Now().Format("150405")
	if err := eng.CreateNetwork(ctx, netName, true, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.RemoveNetwork(ctx, netName) })
	spec, ns := AgentSpec{
		Name: netName + "-c", Image: testImage, User: "1234:1234", IcaroDir: dir,
		Extra:  []Mount{{Source: filepath.Dir(bin), Target: "/opt/icaro", ReadOnly: true}},
		Labels: map[string]string{LabelSession: "it"}, Egress: Attachment{Network: netName},
		Limits: sandbox.Limits{MemoryBytes: 256 << 20, Pids: 64}, Runtime: testRuntime,
	}.Build()
	id, err := eng.CreateContainerOn(ctx, spec, ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.RemoveContainer(ctx, id) })
	if err := eng.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	checkRuntime(t, id)
	script := fmt.Sprintf(`echo '{"jsonrpc":"2.0","id":1,"method":"ping"}' | /opt/icaro/%s mcp --socket %s/mcp.sock`, filepath.Base(bin), IcaroPath)
	out, err := run(t, ExecArgs(ExecOptions{Container: id, User: "1234:1234", Cmd: []string{"sh", "-c", script}})...)
	if err != nil || !strings.Contains(out, `"reached":"host"`) {
		t.Fatalf("the bridge did not reach the session socket: %v %q", err, out)
	}
}

// TestRuntimeMountKinds starts one container per kind of mount an agent
// spec uses and reports which ones testRuntime cannot start. It only
// logs: it is the diagnosis for TestAgentGitProtection under gVisor.
func TestRuntimeMountKinds(t *testing.T) {
	if testRuntime == "" {
		t.Skip("ICARO_TEST_RUNTIME is not set")
	}
	eng := engine(t)
	ctx := context.Background()
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	repo, ws := filepath.Join(root, "repo"), filepath.Join(root, "wt", "s1")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "icaro/s1", ws)
	hh := filepath.Join(root, "hosthome")
	_ = os.MkdirAll(filepath.Join(hh, ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(hh, ".netrc"), []byte("x"), 0o600)
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	limits := sandbox.Limits{MemoryBytes: 256 << 20, Pids: 64}
	cases := map[string]AgentSpec{
		"workspace":   {Workspace: ws},
		"git":         {Workspace: ws, GitDir: filepath.Join(repo, ".git")},
		"git no pids": {Workspace: ws, GitDir: filepath.Join(repo, ".git")},
		"extra":       {Extra: []Mount{{Source: hh, Target: "/h"}}},
		"dir mask":    {Extra: []Mount{{Source: hh, Target: "/h", Masks: []Mask{{Target: "/h/.ssh"}}}}},
		"file mask":   {Extra: []Mount{{Source: hh, Target: "/h", Masks: []Mask{{Target: "/h/.netrc", File: true}}}}},
		"host user":   {},
		"limits only": {},
	}
	for name, a := range cases {
		a.Name = "icaro-it-kind-" + strings.ReplaceAll(name, " ", "-") + "-" + time.Now().Format("150405")
		a.Image, a.Labels, a.Runtime = testImage, map[string]string{LabelSession: "it"}, testRuntime
		a.User = "1234:1234"
		if name != "limits only" {
			a.User = user
		}
		switch name {
		case "host user":
		case "git no pids":
			a.Limits = sandbox.Limits{MemoryBytes: limits.MemoryBytes}
		default:
			a.Limits = limits
		}
		spec, ns := a.Build()
		id, err := eng.CreateContainerOn(ctx, spec, ns)
		if err != nil {
			t.Logf("%-11s create: %v", name, err)
			continue
		}
		err = eng.StartContainer(ctx, id)
		t.Logf("%-11s start: %v", name, err)
		_ = eng.RemoveContainer(ctx, id)
	}
}
