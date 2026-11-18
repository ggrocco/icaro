//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container"
)

// FakeAgentImage is the image the assistant's end-to-end tests run as a
// claude profile: busybox, the icaro binary and testdata/fake-agent/claude,
// which calls the assistant's MCP tools for real over the session socket.
const FakeAgentImage = "icaro-e2e-fake-agent:dev"

// buildFakeAgent builds FakeAgentImage around the linux icaro binary.
func buildFakeAgent(t *testing.T, bin string) {
	t.Helper()
	ctxDir := t.TempDir()
	for _, f := range []string{"Dockerfile", "claude"} {
		b, err := os.ReadFile(filepath.Join("testdata", "fake-agent", f))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(ctxDir, f), string(b))
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "icaro"), b, 0o755); err != nil { //nolint:gosec // an executable in the image
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ctxDir, "claude"), 0o755); err != nil { //nolint:gosec // an executable in the image
		t.Fatal(err)
	}
	if out, err := command("docker", "build", "-q", "-t", FakeAgentImage, ctxDir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
}

// TestAssistant is ADR 0035's flow from the CLI against a real daemon: a
// chat's first message starts an assist container, the agent CLI runs in
// it, calls get_draft and propose_workflow over the session's MCP socket,
// and the proposal becomes the chat's draft; saving it through the chat
// records "<person> via assistant <chat>"; a run the assistant asks for
// starts only after a person approves it.
func TestAssistant(t *testing.T) {
	bin, err := filepath.Abs(envOr("ICARO_E2E_BIN", "../../bin/icaro"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip(bin + " not built; run make build")
	}
	if err := command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	buildFakeAgent(t, bin)
	home, err := os.MkdirTemp("/tmp", "iceas-")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "config", "icaro.yaml"), `version: 1
sessions:
  default_profile: claude
profiles:
  claude:
    agent: claude
    image: `+FakeAgentImage+`
    egress: []
previews:
  enabled: false
assistant:
  idle_timeout: 1m
`)
	env := append(os.Environ(), "ICARO_HOME="+home, "ICARO_PASSWORD=e2e-password", "ICARO_CONFIG=")
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test binary
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("icaro %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}
	t.Cleanup(func() {
		if pid, err := os.ReadFile(filepath.Join(home, "run", "icaro.pid")); err == nil {
			stopDaemon(strings.TrimSpace(string(pid)))
		}
		if out, err := command("docker", "ps", "-aq", "--filter", "label="+container.LabelKind+"=assist").Output(); err == nil {
			for _, id := range strings.Fields(string(out)) {
				_ = command("docker", "rm", "-f", id).Run()
			}
		}
		ns := container.Namespace(home)
		_ = command("docker", "network", "rm", "icaro-egress-"+ns+"-claude").Run()
		_ = os.RemoveAll(home)
	})

	run("init")
	run("serve", "--background")
	chat := strings.TrimSpace(run("assist", "new", "--title", "e2e"))

	// A proposal, made through the real MCP socket.
	out := run("assist", "send", chat, "Make a workflow that says hello")
	for _, want := range []string{"→ get_draft", "→ propose_workflow", "I proposed a hello workflow (valid)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("reply lacks %q:\n%s", want, out)
		}
	}
	var d struct {
		Workflow      string `json:"workflow"`
		SessionID     string `json:"session_id"`
		SessionStatus string `json:"session_status"`
		Draft         struct {
			YAML    string `json:"yaml"`
			Source  string `json:"source"`
			Version int    `json:"version"`
		} `json:"draft"`
	}
	if err := json.Unmarshal([]byte(run("assist", "show", chat, "--json")), &d); err != nil {
		t.Fatal(err)
	}
	if d.Draft.Source != "assistant" || !strings.Contains(d.Draft.YAML, "Proposed by the assistant") || d.Workflow != "hello" || d.SessionStatus != "running" {
		t.Fatalf("chat %+v", d)
	}
	// Proposing saved nothing.
	if out := run("workflow", "list"); strings.Contains(out, "hello") {
		t.Fatalf("propose_workflow saved a workflow:\n%s", out)
	}

	// Saving through the chat attributes the version to the person.
	sock := filepath.Join(home, "run", "icaro.sock")
	body, _ := json.Marshal(map[string]string{"yaml": d.Draft.YAML})
	mcpLike := command("curl", "-sf", "--unix-socket", sock, "-X", "POST", "-H", "Content-Type: application/json",
		"--data-binary", string(body), "http://icaro/api/v1/chats/"+chat+"/apply")
	if out, err := mcpLike.CombinedOutput(); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if out := run("workflow", "versions", "hello"); !strings.Contains(out, "via assistant "+chat) {
		t.Fatalf("version actor:\n%s", out)
	}

	// A run the assistant asks for waits for a person.
	done := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "assist", "send", chat, "please run hello") //nolint:gosec // test binary
		cmd.Env = env
		b, _ := cmd.CombinedOutput()
		done <- string(b)
	}()
	var approval string
	waitFor(t, "an approval card", func() bool {
		var dd struct {
			Approvals []struct {
				ID string `json:"id"`
			} `json:"approvals"`
		}
		_ = json.Unmarshal([]byte(run("assist", "show", chat, "--json")), &dd)
		if len(dd.Approvals) == 1 {
			approval = dd.Approvals[0].ID
			return true
		}
		return false
	})
	if out := run("runs", "list"); strings.Contains(out, "hello") {
		t.Fatalf("a run started before approval:\n%s", out)
	}
	run("assist", "approve", chat, approval)
	if out := <-done; !strings.Contains(out, "The run was started.") {
		t.Fatalf("gated run:\n%s", out)
	}
	if out := run("runs", "list"); !strings.Contains(out, "hello") {
		t.Fatalf("no run after approval:\n%s", out)
	}

	// Deleting the chat removes its container.
	run("assist", "rm", chat, "--yes")
	waitFor(t, "the assist container to go", func() bool {
		out, err := command("docker", "ps", "-aq", "--filter", "label="+container.LabelSession+"="+d.SessionID).Output()
		return err == nil && strings.TrimSpace(string(out)) == ""
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
