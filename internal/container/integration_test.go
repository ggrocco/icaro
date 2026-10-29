//go:build integration

package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
)

// testImage is a small image with sh and wget.
const testImage = "mirror.gcr.io/library/busybox:1.36"

// testRuntime is the OCI runtime the agent containers of these tests run
// on: ICARO_TEST_RUNTIME (runsc in CI's gvisor job), else Docker's
// default. Helper containers (the egress proxy) stay on the default.
var testRuntime = os.Getenv("ICARO_TEST_RUNTIME")

// runtimeArgs are the `docker run` flags for testRuntime and egress
// attachment att, as AgentSpec.Build sets them: on another runtime the
// proxy is named in /etc/hosts.
func runtimeArgs(att Attachment) []string {
	if testRuntime == "" {
		return nil
	}
	args := []string{"--runtime", testRuntime}
	for _, h := range att.Hosts {
		args = append(args, "--add-host", h)
	}
	return args
}

// testPids is the pids limit of a test agent container. gVisor's kernel
// and gofer run as host threads in the container's cgroup, so under
// another runtime the limit also budgets those: keep Ícaro's default.
func testPids() int64 {
	if testRuntime != "" {
		return 0
	}
	return 64
}

// checkRuntime asserts that container id runs on testRuntime: Docker
// recorded it, and under gVisor the container sees gVisor's kernel.
func checkRuntime(t *testing.T, id string) {
	t.Helper()
	if testRuntime == "" {
		return
	}
	out, err := run(t, "inspect", "-f", "{{.HostConfig.Runtime}}", id)
	if err != nil || strings.TrimSpace(out) != testRuntime {
		t.Fatalf("runtime %q, want %q: %v", strings.TrimSpace(out), testRuntime, err)
	}
	if testRuntime == "runsc" {
		// gVisor's sentry logs "Starting gVisor..." at boot and reports
		// kernel release 4.4.0; the host's kernel does neither.
		out, _ := run(t, "exec", id, "sh", "-c", "dmesg 2>/dev/null; uname -r")
		if !strings.Contains(out, "gVisor") && !strings.Contains(out, "4.4.0") {
			state, _ := run(t, "inspect", "-f", "{{.State.Status}} exit={{.State.ExitCode}} error={{.State.Error}}", id)
			logs, _ := run(t, "logs", id)
			t.Fatalf("not running under gVisor: %q\nstate: %s\nlogs: %s", out, state, logs)
		}
	}
}

func engine(t *testing.T) *docker.Engine {
	t.Helper()
	eng, err := docker.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Ping(context.Background()); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// staticIcaro builds a static icaro binary for the helper image.
func staticIcaro(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	out := filepath.Join(t.TempDir(), "icaro")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/icaro")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build icaro: %v\n%s", err, b)
	}
	return out
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// TestEgressConfinement: a container on the profile network reaches an
// allow-listed host through the proxy, is refused for any other host, and
// has no direct route out.
func TestEgressConfinement(t *testing.T) {
	eng := engine(t)
	ctx := context.Background()
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	profile := "it" + strings.ToLower(time.Now().Format("150405"))
	// Two web servers on the default bridge: one allowed, one not.
	var ips []string
	for _, name := range []string{"allowed", "denied"} {
		cname := "icaro-it-" + profile + "-" + name
		if out, err := run(t, "run", "-d", "--name", cname, testImage, "sh", "-c", "mkdir -p /www && echo "+name+" > /www/index.html && httpd -f -p 80 -h /www"); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		t.Cleanup(func() { _, _ = run(t, "rm", "-f", cname) })
		out, err := run(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", cname)
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("ip: %v %q", err, out)
		}
		ips = append(ips, strings.TrimSpace(out))
	}
	eg := &DockerEgress{Engine: eng, HelperBinary: staticIcaro(t)}
	att, err := eg.Ensure(ctx, profile, []string{ips[0]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids, _ := eng.ListByLabel(ctx, map[string]string{LabelEgress: profile})
		for _, id := range ids {
			_ = eng.RemoveContainer(ctx, id)
		}
		_ = eng.RemoveNetwork(ctx, NetworkName(profile))
	})
	// Ensure is idempotent and reuses the proxy.
	if _, err := eg.Ensure(ctx, profile, []string{ips[0]}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := eng.ListByLabel(ctx, map[string]string{LabelEgress: profile}); len(ids) != 1 {
		t.Fatalf("want one proxy, got %d", len(ids))
	}
	time.Sleep(time.Second)

	client := func(script string) (string, error) {
		args := append([]string{"run", "--rm", "--network", att.Network}, runtimeArgs(att)...)
		for _, e := range att.Env {
			args = append(args, "-e", e)
		}
		args = append(args, testImage, "sh", "-c", script)
		return run(t, args...)
	}
	out, err := client("wget -q -T 5 -O - http://" + ips[0] + "/")
	if err != nil || !strings.Contains(out, "allowed") {
		t.Fatalf("allow-listed host must be reachable: %v %q", err, out)
	}
	out, err = client("wget -q -T 5 -O - http://" + ips[1] + "/")
	if err == nil || strings.Contains(out, "denied\n") {
		t.Fatalf("non-allowlisted host must fail: %q", out)
	}
	out, err = client("unset http_proxy HTTP_PROXY; wget -q -T 3 -O - http://" + ips[1] + "/")
	if err == nil {
		t.Fatalf("direct route out must not exist: %q", out)
	}
}

func TestHelperImageImport(t *testing.T) {
	eng := engine(t)
	ref, data, err := HelperImageFromBinary(staticIcaro(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := eng.ImportImage(ctx, bytes.NewReader(data), ref, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ImageDigest(ctx, ref); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "run", "--rm", "--read-only", "--user", "65534:65534", "--entrypoint", "/icaro", ref, "version")
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("%v %q", err, out)
	}
	if _, _, err := HelperImageFromBinary("/bin/sh"); err == nil {
		t.Log("note: /bin/sh is static on this host")
	}
	_ = io.Discard
}

func fileUID(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// TestAgentContainerBaseline runs an agent spec and checks the baseline
// from inside: non-root, read-only root, /workspace writable, home mounted.
func TestAgentContainerBaseline(t *testing.T) {
	eng := engine(t)
	ctx := context.Background()
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	ws, home := t.TempDir(), t.TempDir()
	_ = os.Chmod(ws, 0o777)
	_ = os.Chmod(home, 0o777)
	net := "icaro-it-agent-" + time.Now().Format("150405")
	if err := eng.CreateNetwork(ctx, net, true, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.RemoveNetwork(ctx, net) })
	spec, ns := AgentSpec{
		Name: net + "-c", Image: testImage, User: "1234:1234", Workspace: ws, Home: HomeMount{Dir: home},
		Labels: map[string]string{LabelSession: "it"}, Egress: Attachment{Network: net},
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
	script := `id -u; pwd; touch /workspace/ok && echo ws-ok; touch /home/icaro/h && echo home-ok; (touch /rootfs 2>/dev/null && echo ROOT-WRITABLE) || echo ro-ok`
	out, err := run(t, ExecArgs(ExecOptions{Container: id, User: "1234:1234", Cmd: []string{"sh", "-c", script}})...)
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, want := range []string{"1234\n", "/workspace\n", "ws-ok", "home-ok", "ro-ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	fi, err := os.Stat(filepath.Join(ws, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if uid := fileUID(fi); uid != 1234 {
		t.Fatalf("file written in /workspace is owned by %d, want the session uid", uid)
	}
	// Exit codes propagate through docker exec.
	_, err = run(t, ExecArgs(ExecOptions{Container: id, Cmd: []string{"sh", "-c", "exit 7"}})...)
	if ExitCode(err) != 7 {
		t.Fatalf("exit code %d", ExitCode(err))
	}
}

// TestCreateNetworkRefusesMismatch: an existing network that is not
// internal, or not ours, is refused instead of reused, and the doctor
// check reports it.
func TestCreateNetworkRefusesMismatch(t *testing.T) {
	eng := engine(t)
	ctx := context.Background()
	name := NetworkName("it" + time.Now().Format("150405"))
	if err := eng.CreateNetwork(ctx, name, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.RemoveNetwork(ctx, name) })
	if err := eng.CreateNetwork(ctx, name, true, nil); err == nil || !strings.Contains(err.Error(), "not internal") {
		t.Fatalf("a non-internal network must be refused: %v", err)
	}
	r := EgressNetworkCheck(eng).Run(ctx)
	if r.Severity != "error" || !strings.Contains(r.Detail, name) || r.Fix == nil {
		t.Fatalf("doctor: %+v", r)
	}
	if _, err := r.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateNetwork(ctx, name, true, map[string]string{LabelEgress: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateNetwork(ctx, name, true, map[string]string{LabelEgress: "it"}); err == nil || !strings.Contains(err.Error(), "not created by") {
		t.Fatalf("a foreign network must be refused: %v", err)
	}
	n, err := eng.InspectNetwork(ctx, name)
	if err != nil || !n.Internal {
		t.Fatalf("%+v %v", n, err)
	}
}

// TestAgentGitProtection: from inside the container, nothing the host's
// git executes can be changed and shared-home masks hide credentials,
// while a commit's writes still succeed.
func TestAgentGitProtection(t *testing.T) {
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
	g := filepath.Join(repo, ".git")
	_ = os.RemoveAll(filepath.Join(g, "hooks"))
	sub := filepath.Join(g, "modules", "lib", "x")
	_ = os.MkdirAll(filepath.Join(sub, "objects"), 0o700)
	_ = os.WriteFile(filepath.Join(sub, "HEAD"), []byte("x\n"), 0o600)
	_ = os.WriteFile(filepath.Join(sub, "config"), []byte("x\n"), 0o600)
	a := filepath.Join(g, "worktrees", "s1")
	// A shared-home style mount with a file and a directory mask.
	hh := filepath.Join(root, "hosthome")
	_ = os.MkdirAll(filepath.Join(hh, ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(hh, ".ssh", "id"), []byte("secret"), 0o600)
	_ = os.WriteFile(filepath.Join(hh, ".netrc"), []byte("secret"), 0o600)

	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	spec, ns := AgentSpec{
		Name: "icaro-it-git-" + time.Now().Format("150405"), Image: testImage, User: user,
		Workspace: ws, GitDir: g, Labels: map[string]string{LabelSession: "it"},
		Extra:   []Mount{{Source: hh, Target: "/h", Masks: []Mask{{Target: "/h/.ssh"}, {Target: "/h/.netrc", File: true}}}},
		Limits:  sandbox.Limits{MemoryBytes: 256 << 20, Pids: testPids()},
		Runtime: testRuntime,
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
	denied := []string{
		"echo x >> " + g + "/config", "touch " + g + "/hooks/pre-commit", "mv " + g + "/hooks " + g + "/h2",
		"echo x >> " + a + "/commondir", "echo x >> " + a + "/gitdir", "echo x >> " + a + "/config.worktree",
		"mv " + g + "/worktrees " + g + "/w2", "mv " + a + " " + a + "2", "mkdir " + g + "/worktrees/evil",
		"echo x >> /workspace/.git", "rm /workspace/.git",
		"touch " + sub + "/hooks/post-checkout", "echo x >> " + sub + "/config", "mv " + g + "/modules/lib " + g + "/modules/l2",
		"grep -q secret /h/.netrc", "grep -q secret /h/.ssh/id",
	}
	allowed := []string{
		"touch " + g + "/objects/probe", "touch " + a + "/index.probe", "mkdir -p " + g + "/refs/heads/probe", "touch " + sub + "/objects/probe",
		"touch /h/.ssh/new",  // lands in the mask's tmpfs, not on the host
		"echo x > /h/.netrc", // goes to /dev/null, not to the host file
	}
	var script strings.Builder
	for _, c := range append(append([]string{}, denied...), allowed...) {
		fmt.Fprintf(&script, "if (%s) 2>/dev/null; then echo 'ALLOWED: %s'; else echo 'denied: %s'; fi\n", c, c, c)
	}
	out, err := run(t, ExecArgs(ExecOptions{Container: id, User: user, Cmd: []string{"sh", "-c", script.String()}})...)
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, c := range denied {
		if !strings.Contains(out, "denied: "+c+"\n") {
			t.Errorf("must be denied: %s\n%s", c, out)
		}
	}
	for _, c := range allowed {
		if !strings.Contains(out, "ALLOWED: "+c+"\n") {
			t.Errorf("must be allowed: %s\n%s", c, out)
		}
	}
	if _, err := os.Stat(filepath.Join(hh, ".ssh", "new")); err == nil {
		t.Error("a write under a masked directory reached the host")
	}
	if b, _ := os.ReadFile(filepath.Join(hh, ".netrc")); string(b) != "secret" {
		t.Errorf("a write to a masked file reached the host: %q", b)
	}
}
