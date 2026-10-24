//go:build integration

package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"icaro/internal/container/docker"
)

const testImage = "mirror.gcr.io/library/alpine:3.24"

// command runs docker or the freshly built icaro binary with arguments the
// test controls.
func command(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...) //nolint:gosec // test harness, see above
}

// daemon is a real icaro binary running against a private ICARO_HOME.
type daemon struct {
	t    *testing.T
	bin  string
	home string
	env  []string
	// runIDs started by the test, for container cleanup.
	runIDs []string
}

func newDaemon(t *testing.T) *daemon {
	t.Helper()
	eng, err := docker.New("")
	if err != nil {
		t.Skipf("docker: %v", err)
	}
	ctx := context.Background()
	if err := eng.Ping(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Skipf("image %s: %v", testImage, err)
	}
	_ = eng.Close()
	// A short home keeps the socket path under the sun_path limit.
	home, err := os.MkdirTemp("/tmp", "icaro-chaos-")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "icaro")
	args := []string{"build", "-o", bin}
	if os.Getenv("GOCOVERDIR") != "" { // make cover-e2e
		args = append(args, "-cover", "-coverpkg=icaro/...")
	}
	build := command("go", append(args, "icaro/cmd/icaro")...)
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}
	d := &daemon{t: t, bin: bin, home: filepath.Join(home, "h"), env: append(os.Environ(),
		"ICARO_HOME="+filepath.Join(home, "h"), "ICARO_CONFIG=", "ICARO_PASSWORD=chaos-test-password",
		// Keep the run on the default network so no egress helper image is needed.
		"ICARO_PREVIEWS__ENABLED=false")}
	t.Cleanup(func() {
		d.kill()
		// Remove step containers of this daemon's runs.
		for _, id := range d.runIDs {
			if ids, err := command("docker", "ps", "-aq", "--filter", "label=icaro.run="+id).Output(); err == nil {
				for _, c := range strings.Fields(string(ids)) {
					_ = command("docker", "rm", "-f", c).Run()
				}
			}
		}
		_ = os.RemoveAll(home)
	})
	d.icaro("init")
	return d
}

func (d *daemon) icaro(args ...string) string {
	d.t.Helper()
	out, err := d.try(args...)
	if err != nil {
		d.t.Fatalf("icaro %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (d *daemon) try(args ...string) (string, error) {
	cmd := command(d.bin, args...)
	cmd.Env = d.env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func (d *daemon) start() { d.icaro("serve", "--background") }

func (d *daemon) pid() int {
	b, err := os.ReadFile(filepath.Join(d.home, "run", "icaro.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// kill SIGKILLs the daemon: no shutdown path runs.
func (d *daemon) kill() {
	if pid := d.pid(); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

type runView struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Steps  []struct {
		ID      string         `json:"id"`
		Status  string         `json:"status"`
		Attempt int            `json:"attempt"`
		Outputs map[string]any `json:"outputs"`
	} `json:"steps"`
}

func (d *daemon) run(id string) runView {
	d.t.Helper()
	var v runView
	if err := json.Unmarshal([]byte(d.icaro("runs", "get", id, "--json")), &v); err != nil {
		d.t.Fatal(err)
	}
	return v
}

func (d *daemon) waitStep(id, step, status string, timeout time.Duration) runView {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v := d.run(id)
		for _, s := range v.Steps {
			if s.ID == step && s.Status == status {
				return v
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("step %s never reached %s: %+v", step, status, v)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitStepContainer waits until a step container of run id is running
// and returns the running containers. The engine records a step as
// running before it starts the container (so a crash in between fails
// that attempt instead of starting a duplicate); a test that kills the
// daemon to exercise re-attachment must wait for the container itself.
func (d *daemon) waitStepContainer(id string, timeout time.Duration) []string {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := command("docker", "ps", "-q", "--filter", "label=icaro.run="+id, "--filter", "status=running").Output()
		if ids := strings.Fields(string(out)); err == nil && len(ids) > 0 {
			return ids
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("no running step container for run %s: %v %q", id, err, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (d *daemon) waitTerminal(id string, timeout time.Duration) runView {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v := d.run(id)
		switch v.Status {
		case "succeeded", "failed", "cancelled":
			return v
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("run %s stuck in %s: %+v", id, v.Status, v)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (d *daemon) apply(yaml string) {
	d.t.Helper()
	p := filepath.Join(d.home, "wf.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		d.t.Fatal(err)
	}
	d.icaro("workflow", "apply", p)
}

func (d *daemon) startRun(name string) string {
	d.t.Helper()
	out := d.icaro("run", name, "--json")
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.ID == "" {
		d.t.Fatalf("run %s: %q %v", name, out, err)
	}
	d.runIDs = append(d.runIDs, v.ID)
	return v.ID
}

const slowWorkflow = `version: 1
name: chaos-slow
steps:
  - id: slow
    image: ` + testImage + `
    command: |
      sleep 4
      echo '{"done":true}' > /icaro/output.json
    outputs:
      done: json:done
  - id: after
    image: ` + testImage + `
    needs: [slow]
    command: echo after
`

// A SIGKILLed daemon leaves the step container running; the restarted
// daemon re-attaches to it and the run finishes instead of hanging or
// starting a duplicate.
func TestDaemonKilledMidRunReattaches(t *testing.T) {
	d := newDaemon(t)
	d.start()
	d.apply(slowWorkflow)
	id := d.startRun("chaos-slow")
	d.waitStep(id, "slow", "running", 60*time.Second)
	d.waitStepContainer(id, 30*time.Second)
	d.kill()

	// While the daemon is down the run is still "running" in the store;
	// doctor reports the stale lock/socket and the CLI restarts the daemon.
	if out, _ := d.try("doctor"); !strings.Contains(out, "stale") {
		t.Logf("doctor while down:\n%s", out)
	}
	d.start()
	v := d.waitTerminal(id, 90*time.Second)
	if v.Status != "succeeded" {
		t.Fatalf("after restart: %+v", v)
	}
	for _, s := range v.Steps {
		if s.ID == "slow" && (s.Attempt != 1 || s.Outputs["done"] != true) {
			t.Fatalf("slow step was not re-attached (attempt %d, outputs %v)", s.Attempt, s.Outputs)
		}
	}
}

// If the step container disappears while the daemon is down, the
// restarted daemon fails that attempt deterministically (no retry here),
// and the run ends instead of staying ambiguous.
func TestDaemonKilledAndContainerGoneFailsDeterministically(t *testing.T) {
	d := newDaemon(t)
	d.start()
	d.apply(slowWorkflow)
	id := d.startRun("chaos-slow")
	d.waitStep(id, "slow", "running", 60*time.Second)
	ids := d.waitStepContainer(id, 30*time.Second)
	d.kill()
	for _, c := range ids {
		_ = command("docker", "rm", "-f", c).Run()
	}
	d.start()
	v := d.waitTerminal(id, 90*time.Second)
	if v.Status != "failed" {
		t.Fatalf("want failed after the container vanished, got %+v", v)
	}
	for _, s := range v.Steps {
		if s.ID == "after" && s.Status == "succeeded" {
			t.Fatal("a dependent of the lost step must not run")
		}
	}
}
