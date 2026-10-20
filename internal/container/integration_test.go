//go:build integration

package container

import (
	"bytes"
	"context"
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
		args := []string{"run", "--rm", "--network", att.Network}
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
	if err := eng.EnsureNetwork(ctx, net, true, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.RemoveNetwork(ctx, net) })
	spec, ns := AgentSpec{
		Name: net + "-c", Image: testImage, User: "1234:1234", Workspace: ws, Home: HomeMount{Dir: home},
		Labels: map[string]string{LabelSession: "it"}, Egress: Attachment{Network: net},
		Limits: sandbox.Limits{MemoryBytes: 256 << 20, Pids: 64},
	}.Build()
	id, err := eng.CreateContainerOn(ctx, spec, ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.RemoveContainer(ctx, id) })
	if err := eng.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
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
