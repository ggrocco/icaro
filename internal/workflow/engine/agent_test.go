package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/assist/stream"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/serial"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// fixtureStream reads a modeled CLI stream recorded beside the stream
// parser (spike S11 records the real ones next to them).
func fixtureStream(t *testing.T, agent, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "assist", "stream", "testdata", agent, "modeled", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeAgents is the session manager's agent-step seam.
type fakeAgents struct {
	ops      serial.Queue
	digests  int
	started  []session.AgentStepRequest
	finished []string
	resumed  []string
	imageErr error
	startErr error
}

func newFakeAgents() *fakeAgents { return &fakeAgents{ops: serial.New()} }

func (f *fakeAgents) AgentImage(_ context.Context, profile string) (string, error) {
	var d string
	err := f.imageErr
	f.ops.Do(func() {
		f.digests++
		// The tag "moves" on every resolution: a run must keep the first.
		d = fmt.Sprintf("reg/%s@sha256:%064d", profile, f.digests)
	})
	return d, err
}

func (f *fakeAgents) StartAgentStep(_ context.Context, req session.AgentStepRequest) (*session.AgentStep, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	var id string
	f.ops.Do(func() {
		f.started = append(f.started, req)
		id = fmt.Sprintf("step-%d", len(f.started))
	})
	return f.step(id, req.Profile, req.Memory), nil
}

func (f *fakeAgents) step(id, profile string, memory bool) *session.AgentStep {
	as := &session.AgentStep{
		SessionID: id, Agent: "claude", Home: container.HomeMount{Volume: "icaro-home-" + id},
		Env:    []string{"ICARO_SESSION=" + id, "ANTHROPIC_API_KEY=icaro-inject." + id + ".token", "ANTHROPIC_BASE_URL=http://icaro-egress:3129/anthropic"},
		Egress: container.Attachment{Network: "icaro-egress-" + profile, Env: []string{"HTTPS_PROXY=http://agent-proxy:3128"}, Hosts: []string{"icaro-egress:10.0.0.9"}},
	}
	if memory {
		as.MCPSocket = "/run/icaro/sessions/" + id + "/mcp.sock"
	}
	return as
}

func (f *fakeAgents) ResumeAgentStep(_ context.Context, id string) (*session.AgentStep, error) {
	f.ops.Do(func() { f.resumed = append(f.resumed, id) })
	return f.step(id, "reviewer", true), nil
}

func (f *fakeAgents) FinishAgentStep(_ context.Context, id string) error {
	f.ops.Do(func() { f.finished = append(f.finished, id) })
	return nil
}

func (f *fakeAgents) counts() (started, finished int) {
	f.ops.Do(func() { started, finished = len(f.started), len(f.finished) })
	return
}

func agentFixture(t *testing.T) (*fixture, *fakeAgents) {
	f := newFixture(t)
	ag := newFakeAgents()
	f.deps.Agents = ag
	f.deps.Egress = &stubEgress{}
	f.cfg.AgentProfiles = []workflow.AgentProfile{{Name: "reviewer", Agent: "claude"}, {Name: "coder", Agent: "codex"}}
	f.deps.Secrets = fakeSecrets{"DEPLOY": "s3cr3t-deploy-value"}
	return f, ag
}

const reviewWorkflow = `
version: 1
name: review
env: {BRANCH: main}
steps:
  - id: review
    agent:
      prompt: "Review ${{ env.BRANCH }} for run ${{ run.id }}"
      params: {model: sonnet, max_turns: 30}
    env: {TOKEN: "${{ secrets.DEPLOY }}"}
    outputs: {summary: "json:summary"}
  - id: second
    needs: [review]
    agent: {prompt: "again", memory: false}
  - id: post
    needs: [second]
    image: img
    command: echo "${{ steps.review.outputs.result }} / ${{ steps.review.outputs.summary }}"
`

func TestAgentStep(t *testing.T) {
	f, ag := agentFixture(t)
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		switch c.Spec.Labels[LabelStep] {
		case "review":
			return docker.Behavior{Stdout: fixtureStream(t, "claude", "step-edit"), Stderr: "warning: s3cr3t-deploy-value\n", Output: []byte(`{"summary":"no risks"}`)}
		case "second":
			return docker.Behavior{Stdout: fixtureStream(t, "claude", "step-output")}
		}
		return docker.Behavior{}
	}
	run := f.enqueue(reviewWorkflow, nil)
	_, stop := f.start()
	defer stop()
	if got := f.wait(run.ID, store.RunSucceeded, store.RunFailed); got.Status != store.RunSucceeded {
		t.Fatalf("%+v %s", got, f.dump(run.ID))
	}
	c := f.containersForStep("review")[0]
	s := c.Spec
	if !strings.HasPrefix(s.Image, "reg/reviewer@sha256:") {
		t.Fatalf("image %q", s.Image)
	}
	if strings.Join(s.Entrypoint, " ") != `/bin/sh -c exec "$@" < /icaro/prompt.md agent` ||
		!strings.HasPrefix(strings.Join(s.Cmd, " "), "claude -p --output-format stream-json") || !strings.HasSuffix(strings.Join(s.Cmd, " "), "--model sonnet --max-turns 30") {
		t.Fatalf("command %v %v", s.Entrypoint, s.Cmd)
	}
	if got := string(c.Files["/icaro/prompt.md"]); got != "Review main for run "+run.ID {
		t.Fatalf("prompt %q", got)
	}
	env := strings.Join(s.Env, " ")
	if !strings.Contains(env, "HOME=/home/icaro") || !strings.Contains(env, "HTTPS_PROXY=http://agent-proxy:3128") || strings.Contains(env, "http://proxy:3128") ||
		!strings.Contains(env, "TOKEN=s3cr3t-deploy-value") || !strings.Contains(env, "ANTHROPIC_BASE_URL=") {
		t.Fatalf("env %v", s.Env)
	}
	if s.HostConfig.NetworkMode != "icaro-egress-reviewer" {
		t.Fatalf("network %q", s.HostConfig.NetworkMode)
	}
	mounts := map[string]string{}
	for _, m := range s.HostConfig.Mounts {
		mounts[m.Target] = m.Source
	}
	if mounts["/home/icaro"] != "icaro-home-step-1" || mounts["/icaro/mcp.sock"] != "/run/icaro/sessions/step-1/mcp.sock" || mounts["/icaro"] == "" {
		t.Fatalf("mounts %v", mounts)
	}
	// An injected prompt has nothing else to reach: no Docker or daemon
	// socket, no host directory but the workspace, and no secret but the
	// one the step passed in env.
	if len(mounts) != 4 || mounts["/workspace"] == "" {
		t.Fatalf("an agent step mounts only its workspace, I/O, home and memory socket: %v", mounts)
	}
	for _, kv := range s.Env {
		if strings.Contains(kv, "s3cr3t") && !strings.HasPrefix(kv, "TOKEN=") {
			t.Fatalf("secret in %s", kv)
		}
	}
	if s.Labels[LabelSession] != "step-1" {
		t.Fatal("label")
	}
	// The second agent step has its own session and home, no memory, and
	// the same pinned image.
	s2 := f.containersForStep("second")[0].Spec
	m2 := map[string]string{}
	for _, m := range s2.HostConfig.Mounts {
		m2[m.Target] = m.Source
	}
	if m2["/home/icaro"] != "icaro-home-step-2" || m2["/icaro/mcp.sock"] != "" || s2.Image != s.Image {
		t.Fatalf("second %v %s", m2, s2.Image)
	}
	if ag.digests != 1 {
		t.Fatalf("the image was resolved %d times", ag.digests)
	}
	if started, finished := ag.counts(); started != 2 || finished != 2 || !ag.started[0].Memory || ag.started[1].Memory {
		t.Fatalf("step sessions %+v %v", ag.started, ag.finished)
	}
	row := f.step(run.ID, "review")
	if row.Profile != "reviewer" || row.ImageDigest != s.Image || row.Usage.Input != 2400 || row.Usage.Output != 310 || row.Usage.CacheRead != 800 || row.Usage.CostMicro != 42100 || row.SessionID != "step-1" {
		t.Fatalf("row %+v", row)
	}
	out := f.outputs(run.ID, "review")
	if out["result"] != "Fixed the off-by-one in src/app.go; no other risks." || out["summary"] != "no risks" {
		t.Fatalf("outputs %v", out)
	}
	if got := strings.Join(f.containersForStep("post")[0].Spec.Cmd, " "); !strings.Contains(got, "Fixed the off-by-one") || !strings.Contains(got, "/ no risks") {
		t.Fatalf("a later step reads result and the declared output: %s", got)
	}
	log, _ := os.ReadFile(row.LogPath)
	for _, want := range []string{"I'll read the change first.", "→ Bash ", "← Edit The file", "usage: 3200 input, 310 output tokens, ~$0.0421", "done", "stderr: warning: ***"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(string(log), "s3cr3t") || strings.Contains(string(log), `"type":"assistant"`) {
		t.Fatalf("raw or secret log:\n%s", log)
	}
	if p, _ := os.ReadFile(strings.TrimSuffix(row.LogPath, ".log") + ".prompt.md"); string(p) != "Review main for run "+run.ID {
		t.Fatalf("kept prompt %q", p)
	}
	// An image step of the same run keeps the run profile's egress.
	if post := f.containersForStep("post")[0].Spec; post.HostConfig.NetworkMode != "icaro-egress-workflow" {
		t.Fatalf("image step network %q", post.HostConfig.NetworkMode)
	}
}

func TestAgentStepFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stream   string
		exit     int
		retry    int
		attempts int
		want     string
	}{
		{"auth", "auth", 1, 2, 1, "Sign in required: the agent's login for profile reviewer expired or is missing; run `icaro login reviewer`"},
		{"maxturns", "step-maxturns", 1, 1, 2, "the agent failed: error max turns"},
		{"ratelimit", "ratelimit", 1, 0, 1, "usage limit"},
		{"cut", "cut", 0, 0, 1, "the agent exited without finishing its task"},
		{"nonzero", "cut", 3, 0, 1, "the agent exited without finishing (exit code 3)"},
		{"done-but-failed", "step-output", 2, 0, 1, "the agent finished but exited with exit code 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ag := agentFixture(t)
			stream := fixtureStream(t, "claude", tc.stream)
			f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Stdout: stream, ExitCode: tc.exit} }
			run := f.enqueue(fmt.Sprintf("version: 1\nname: fail\nsteps:\n  - id: review\n    retry: %d\n    agent: {prompt: x}\n", tc.retry), nil)
			_, stop := f.start()
			defer stop()
			f.wait(run.ID, store.RunFailed)
			row := f.step(run.ID, "review")
			if !strings.Contains(row.Error, tc.want) || row.Attempt != tc.attempts {
				t.Fatalf("%+v", row)
			}
			if started, finished := ag.counts(); started != tc.attempts || finished != tc.attempts {
				t.Fatalf("each attempt has its own session: %d %d", started, finished)
			}
		})
	}
}

func TestAgentStepConfigErrors(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fixture, *fakeAgents)
		yaml  string
		want  string
	}{
		{"unlisted", func(f *fixture, _ *fakeAgents) {}, "agent: {profile: gone, prompt: x}", `profile "gone" is no longer listed in workflows.agent_profiles`},
		{"off", func(f *fixture, _ *fakeAgents) { f.cfg.AgentProfiles = nil }, "agent: {prompt: x}", "agent steps are off"},
		{"no manager", func(f *fixture, _ *fakeAgents) { f.deps.Agents = nil }, "agent: {prompt: x}", "agent steps need the session manager"},
		{"bad agent", func(f *fixture, _ *fakeAgents) {
			f.cfg.AgentProfiles = []workflow.AgentProfile{{Name: "sh", Agent: ""}}
		}, "agent: {prompt: x}", "cannot run workflow steps"},
		{"unknown agent", func(f *fixture, _ *fakeAgents) {
			f.cfg.AgentProfiles = []workflow.AgentProfile{{Name: "x", Agent: "vim"}}
		}, "agent: {prompt: x}", `unknown agent "vim"`},
		{"param", func(f *fixture, _ *fakeAgents) {}, "agent: {profile: coder, prompt: x, params: {max_turns: 3}}", `unknown param "max_turns"`},
		{"prompt", func(f *fixture, _ *fakeAgents) {}, "agent: {prompt: \"${{ trigger.missing.deep }}\"}", "prompt:"},
		{"start", func(_ *fixture, ag *fakeAgents) { ag.startErr = errors.New("seed busy") }, "agent: {prompt: x}", "agent profile reviewer: seed busy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ag := agentFixture(t)
			tc.setup(f, ag)
			run := f.enqueue("version: 1\nname: cfg\nsteps:\n  - id: step\n    "+tc.yaml+"\n", nil)
			_, stop := f.start()
			defer stop()
			f.wait(run.ID, store.RunFailed)
			if row := f.step(run.ID, "step"); !strings.Contains(row.Error, tc.want) {
				t.Fatalf("%+v", row)
			}
		})
	}
	// An image that cannot be resolved fails the run before any step.
	f, ag := agentFixture(t)
	ag.imageErr = errors.New("no such image")
	run := f.enqueue("version: 1\nname: img\nsteps:\n  - id: step\n    agent: {prompt: x}\n", nil)
	_, stop := f.start()
	defer stop()
	if got := f.wait(run.ID, store.RunFailed); !strings.Contains(got.Error, "agent profile reviewer: no such image") {
		t.Fatalf("%+v", got)
	}
}

// A restart mid-step re-attaches to the agent container, resumes its step
// session and captures its result.
func TestAgentStepReattach(t *testing.T) {
	f, ag := agentFixture(t)
	stream := fixtureStream(t, "claude", "step-edit")
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior {
		return docker.Behavior{Duration: 600 * time.Millisecond, Stdout: stream}
	}
	run := f.enqueue("version: 1\nname: survive\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
	_, stop := f.start()
	for f.step(run.ID, "review").Status != store.StepRunning {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if _, finished := ag.counts(); finished != 0 {
		t.Fatal("a shutdown must leave the step session for re-attach")
	}
	_, stop2 := f.start()
	defer stop2()
	if got := f.wait(run.ID, store.RunSucceeded, store.RunFailed); got.Status != store.RunSucceeded {
		t.Fatalf("%+v %s", got, f.dump(run.ID))
	}
	if n := len(f.containersForStep("review")); n != 1 {
		t.Fatalf("%d containers", n)
	}
	if out := f.outputs(run.ID, "review"); !strings.HasPrefix(out["result"].(string), "Fixed the off-by-one") {
		t.Fatalf("%v", out)
	}
	if len(ag.resumed) != 1 || ag.resumed[0] != "step-1" || ag.digests != 1 {
		t.Fatalf("resumed %v, digests %d", ag.resumed, ag.digests)
	}
	if _, finished := ag.counts(); finished != 1 {
		t.Fatal("finished after re-attach")
	}
}

func TestAgentHelpers(t *testing.T) {
	b := &cappedBuffer{limit: 4}
	_, _ = b.Write([]byte("ab"))
	_, _ = b.Write([]byte("cdef"))
	_, _ = b.Write([]byte("g"))
	if b.String() != "abcd" || !b.over {
		t.Fatal(b.String())
	}
	if formatEvent(streamEvent("usage")) != nil {
		t.Fatal("usage without numbers")
	}
	if l := formatEvent(streamEvent("weird")); l != nil {
		t.Fatal(l)
	}
}

func streamEvent(kind string) stream.Event { return stream.Event{Kind: stream.Kind(kind)} }

// copyHomeAgents gives steps a copy-mode home and a gVisor runtime.
type copyHomeAgents struct{ *fakeAgents }

func (c copyHomeAgents) StartAgentStep(ctx context.Context, req session.AgentStepRequest) (*session.AgentStep, error) {
	as, err := c.fakeAgents.StartAgentStep(ctx, req)
	if as != nil {
		as.Home, as.Runtime = container.HomeMount{Dir: "/home/me/.icaro/home/reviewer/sessions/x/home"}, "runsc"
	}
	return as, err
}

func TestAgentStepCopyHomeAndEvents(t *testing.T) {
	f, ag := agentFixture(t)
	f.deps.Agents = copyHomeAgents{ag}
	tools := fixtureStream(t, "claude", "tools")
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior {
		return docker.Behavior{Stdout: tools, Stderr: strings.Repeat("x", maxStderr+10) + "\n"}
	}
	run := f.enqueue("version: 1\nname: events\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	s := f.containersForStep("review")[0].Spec
	var home string
	for _, m := range s.HostConfig.Mounts {
		if m.Target == "/home/icaro" {
			home = m.Source
		}
	}
	if home != "/home/me/.icaro/home/reviewer/sessions/x/home" || s.HostConfig.Runtime != "runsc" || strings.Join(s.HostConfig.ExtraHosts, " ") != "icaro-egress:10.0.0.9" {
		t.Fatalf("home %q runtime %q hosts %v", home, s.HostConfig.Runtime, s.HostConfig.ExtraHosts)
	}
	log, _ := os.ReadFile(f.step(run.ID, "review").LogPath)
	for _, want := range []string{"→ icaro.get_workflow_schema {}", "← error: Bash Bash is not allowed", "stderr: (truncated)"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if out := f.outputs(run.ID, "review"); out["result"] != "The draft needs a step; I proposed one." {
		t.Fatalf("%v", out)
	}
}

func TestAgentStepImageAndReattachErrors(t *testing.T) {
	f, _ := agentFixture(t)
	f.docker.PullErr = errors.New("pull denied")
	run := f.enqueue("version: 1\nname: pull\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
	_, stop := f.start()
	f.wait(run.ID, store.RunFailed)
	stop()
	if row := f.step(run.ID, "review"); !strings.Contains(row.Error, "pull denied") {
		t.Fatalf("%+v", row)
	}

	// After a restart the container is gone: the attempt fails, and its
	// step session is finished.
	f, ag := agentFixture(t)
	run = f.enqueue("version: 1\nname: gone\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		t.Fatal(err)
	}
	row := f.step(run.ID, "review")
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.SessionID, row.StartedAt = store.StepRunning, 1, "vanished", "step-9", &now
	row.Profile, row.ImageDigest = "reviewer", "reg/reviewer@sha256:pinned"
	if err := f.st.UpdateStepRun(f.ctx, row); err != nil {
		t.Fatal(err)
	}
	_, stop = f.start()
	defer stop()
	f.wait(run.ID, store.RunFailed)
	if r := f.step(run.ID, "review"); !strings.Contains(r.Error, "disappeared") || r.ImageDigest != "reg/reviewer@sha256:pinned" {
		t.Fatalf("%+v", r)
	}
	if ag.digests != 0 || len(ag.resumed) != 1 || len(ag.finished) != 1 || ag.finished[0] != "step-9" {
		t.Fatalf("digests %d resumed %v finished %v", ag.digests, ag.resumed, ag.finished)
	}
}

func TestAgentStepDockerFaults(t *testing.T) {
	for _, op := range []string{"CreateContainer", "CopyIn", "StartContainer", "CreateVolume", "Logs"} {
		t.Run(op, func(t *testing.T) {
			f, ag := agentFixture(t)
			f.deps.Docker = &faultDocker{Fake: f.docker, fail: map[string]error{op: errors.New(op + " failed")}}
			stream := fixtureStream(t, "claude", "step-edit")
			f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Stdout: stream} }
			run := f.enqueue("version: 1\nname: fault\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
			_, stop := f.start()
			defer stop()
			f.wait(run.ID, store.RunFailed)
			row := f.step(run.ID, "review")
			want := op + " failed"
			if op == "Logs" {
				want = "the agent exited without finishing its task"
			}
			if !strings.Contains(row.Error, want) {
				t.Fatalf("%+v", row)
			}
			if started, finished := ag.counts(); started != 1 || finished != 1 {
				t.Fatalf("the step session is always finished: %d %d", started, finished)
			}
		})
	}
}

func TestAgentStepRenderErrorsAndUnstarted(t *testing.T) {
	f, _ := agentFixture(t)
	run := f.enqueue("version: 1\nname: render\nsteps:\n  - id: review\n    env: {X: \"${{ trigger.a.b }}\"}\n    agent: {prompt: x}\n", nil)
	_, stop := f.start()
	f.wait(run.ID, store.RunFailed)
	stop()
	if row := f.step(run.ID, "review"); !strings.Contains(row.Error, "env.X") {
		t.Fatalf("%+v", row)
	}

	// The previous daemon created the container but stopped before
	// starting it.
	f, ag := agentFixture(t)
	run = f.enqueue("version: 1\nname: unstarted\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		t.Fatal(err)
	}
	f.docker.AddContainer("never", docker.Spec{}, docker.Behavior{}, false)
	row := f.step(run.ID, "review")
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.SessionID, row.StartedAt = store.StepRunning, 1, "never", "step-7", &now
	if err := f.st.UpdateStepRun(f.ctx, row); err != nil {
		t.Fatal(err)
	}
	_, stop = f.start()
	defer stop()
	f.wait(run.ID, store.RunFailed)
	if r := f.step(run.ID, "review"); !strings.Contains(r.Error, "before the container started") {
		t.Fatalf("%+v", r)
	}
	if len(ag.finished) != 1 {
		t.Fatal("finished")
	}
}

func TestAgentStepReattachProbeAndLeftovers(t *testing.T) {
	// The probe of the old container fails after a restart.
	f, _ := agentFixture(t)
	f.deps.Docker = &faultDocker{Fake: f.docker, fail: map[string]error{"ContainerStarted": errors.New("probe failed")}}
	run := f.enqueue("version: 1\nname: probe\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		t.Fatal(err)
	}
	row := f.step(run.ID, "review")
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.SessionID, row.StartedAt = store.StepRunning, 1, "old", "step-1", &now
	if err := f.st.UpdateStepRun(f.ctx, row); err != nil {
		t.Fatal(err)
	}
	_, stop := f.start()
	f.wait(run.ID, store.RunFailed)
	stop()
	if r := f.step(run.ID, "review"); !strings.Contains(r.Error, "probe failed") {
		t.Fatalf("%+v", r)
	}

	// A container a crash left for this exact attempt is removed first.
	f, _ = agentFixture(t)
	stream := fixtureStream(t, "claude", "step-edit")
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Stdout: stream} }
	run = f.enqueue("version: 1\nname: leftover\nsteps:\n  - id: review\n    agent: {prompt: x}\n", nil)
	left := f.docker.AddContainer("left", docker.Spec{Labels: map[string]string{LabelRun: run.ID, LabelStep: "review", LabelAttempt: "1"}}, docker.Behavior{}, false)
	_, stop = f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	if !left.Removed {
		t.Fatal("leftover kept")
	}
}

func TestFormatEventClips(t *testing.T) {
	got := formatEvent(stream.Event{Kind: stream.KindToolCall, Tool: "Bash", Args: []byte(`{"command":"` + strings.Repeat("a", 400) + `"}`)})
	if len(got) != 1 || !strings.HasSuffix(got[0], "…") || len(got[0]) > 320 {
		t.Fatal(got)
	}
}
