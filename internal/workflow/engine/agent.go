package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"icaro/internal/assist/stream"
	icontainer "icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
	"icaro/internal/creds"
	"icaro/internal/expr"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

// Agent steps (ADR 0037): one container in the run session, like an image
// step, with three differences. It runs under its agent profile's step
// session: a fresh home layer over the profile's seed per attempt, the
// profile's egress (and credential injection), and the profile's image at
// the digest pinned when the run started. The prompt is fed on stdin from
// /icaro/prompt.md; the CLI's event stream is parsed, capped and redacted
// into the step log, and its final answer becomes the output `result`.

const (
	promptFile = "prompt.md"
	// maxResult caps the result output, like /icaro/output.json.
	maxResult = 1 << 20
	// maxStderr caps the agent's stderr kept for the log.
	maxStderr = 64 << 10
)

// errNoRetry marks an attempt failure that retrying cannot fix (an expired
// login).
var errNoRetry = errors.New("not retried")

// AgentProfile is the engine's view of workflows.agent_profiles.
type AgentProfile = workflow.AgentProfile

// agentProfile resolves the step's profile against the configured list,
// which may have changed since the workflow was saved.
func (rx *runExec) agentProfile(st *workflow.StepSpec) (AgentProfile, error) {
	opts := &workflow.AgentOptions{Profiles: rx.e.cfg.AgentProfiles}
	p, ok := opts.AgentStepProfile(st.Agent)
	if !ok {
		name := st.Agent.Profile
		if name == "" {
			return AgentProfile{}, fmt.Errorf("%w: agent steps are off: no profile is listed in workflows.agent_profiles", errNoRetry)
		}
		return AgentProfile{}, fmt.Errorf("%w: profile %q is no longer listed in workflows.agent_profiles", errNoRetry, name)
	}
	return p, nil
}

// pinAgentImages resolves the image of every agent profile the run uses,
// once per run: a later attempt, or another agent step under the same
// profile, runs the same digest even if the tag moves. After a restart the
// digests recorded on the run's step rows win.
func (rx *runExec) pinAgentImages(ctx context.Context) error {
	rx.images = map[string]string{}
	for _, r := range rx.rows {
		if r.Profile != "" && r.ImageDigest != "" {
			rx.images[r.Profile] = r.ImageDigest
		}
	}
	if rx.e.d.Agents == nil {
		return nil
	}
	for i := range rx.spec.Steps {
		st := &rx.spec.Steps[i]
		if st.Kind() != workflow.KindAgent {
			continue
		}
		p, err := rx.agentProfile(st)
		if err != nil || rx.images[p.Name] != "" {
			continue // the step fails with the reason when it runs
		}
		digest, err := rx.e.d.Agents.AgentImage(ctx, p.Name)
		if err != nil {
			return fmt.Errorf("agent profile %s: %w", p.Name, err)
		}
		rx.images[p.Name] = digest
	}
	return nil
}

// agentOutcome is what an agent's event stream came to.
type agentOutcome struct {
	terminal *stream.Event
	result   string
	usage    stream.Usage
}

// agentAttempt runs (or, with reattach, resumes) one agent-step attempt.
func (rx *runExec) agentAttempt(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, env expr.Env, attempt int, reattach bool) (map[string]any, error) {
	e := rx.e
	if e.d.Agents == nil {
		return nil, fmt.Errorf("%w: agent steps need the session manager", errNoRetry)
	}
	prof, err := rx.agentProfile(st)
	if err != nil {
		row.Attempt = attempt
		return nil, err
	}
	agent, err := creds.LookupAgent(prof.Agent)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoRetry, err)
	}
	spec, err := agent.Step()
	if err != nil {
		return nil, fmt.Errorf("%w: profile %s: %v", errNoRetry, prof.Name, err)
	}
	cmd, err := spec.Command(st.Agent.Params)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoRetry, err)
	}
	// The prompt never reads secrets (validation refuses them; they are
	// not even in its environment).
	penv := expr.Env{}
	for k, v := range env {
		if k != "secrets" {
			penv[k] = v
		}
	}
	prompt, err := expr.RenderString(st.Agent.Prompt, penv)
	if err != nil {
		row.Attempt = attempt
		return nil, fmt.Errorf("prompt: %w", err)
	}
	r, err := rx.render(ctx, st, env, attempt)
	if err != nil {
		row.Attempt = attempt
		return nil, err
	}
	// prepare pinned every listed profile's image, or failed the run.
	row.Profile, row.ImageDigest = prof.Name, rx.images[prof.Name]

	ioVol := ioVolumeName(rx.run.ID, st.ID, attempt)
	var as *session.AgentStep
	finished := false
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if row.ContainerID != "" {
			_ = e.d.Docker.RemoveContainer(cctx, row.ContainerID)
		}
		_ = e.d.Docker.RemoveVolume(cctx, ioVol)
		if as != nil && !finished {
			finished = true
			if err := e.d.Agents.FinishAgentStep(cctx, as.SessionID); err != nil {
				e.log.Warn("finish agent-step session", "run", rx.run.ID, "step", st.ID, "err", err)
			}
		}
	}

	if reattach {
		exists, started, err := e.d.Docker.ContainerStarted(ctx, row.ContainerID)
		if err != nil {
			return nil, err
		}
		if as, err = e.d.Agents.ResumeAgentStep(ctx, row.SessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
			return nil, err
		}
		if !exists || !started {
			cleanup()
			if !exists {
				return nil, errors.New("container disappeared while the daemon was down")
			}
			return nil, errors.New("the daemon stopped before the container started")
		}
		e.log.Info("re-attached to agent step container", "run", rx.run.ID, "step", st.ID, "container", row.ContainerID)
	} else {
		as, err = e.d.Agents.StartAgentStep(ctx, session.AgentStepRequest{
			RunSessionID: rx.ws.SessionID, RunID: rx.run.ID, StepID: st.ID, Attempt: attempt,
			Profile: prof.Name, Workspace: r.dir, Memory: st.Agent.MemoryOn(),
		})
		if err != nil {
			row.Attempt = attempt
			return nil, fmt.Errorf("agent profile %s: %w", prof.Name, err)
		}
		if err := rx.createAgentContainer(ctx, st, row, r, as, cmd, prompt, attempt, ioVol); err != nil {
			cleanup()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
	}

	lc, err := e.openLog(ctx, rx, row, attempt, reattach)
	if err != nil {
		cleanup()
		return nil, err
	}
	streamCtx, stopStream := context.WithCancel(context.WithoutCancel(ctx))
	outcome := make(chan agentOutcome, 1)
	go func() { outcome <- rx.readAgentStream(streamCtx, row.ContainerID, agent.Name, lc) }()
	finishLogs := func(wait time.Duration) agentOutcome {
		var out agentOutcome
		select {
		case out = <-outcome:
		case <-time.After(wait):
			stopStream()
			out = <-outcome
		}
		stopStream()
		lc.close()
		return out
	}

	exitCode, stepErr := rx.waitContainer(ctx, st, row)
	if errors.Is(stepErr, errShutdown) {
		// Daemon stopping: leave the container and its step session for
		// re-attach.
		finishLogs(0)
		return nil, errShutdown
	}
	out := finishLogs(10 * time.Second)
	row.ExitCode = &exitCode
	row.LogSize, row.LogLines = lc.offset, lc.lines
	row.Usage.Input += out.usage.Input
	row.Usage.Output += out.usage.Output
	row.Usage.CacheRead += out.usage.CacheRead
	row.Usage.CacheWrite += out.usage.CacheWrite
	row.Usage.CostMicro += int64(out.usage.CostUSD * 1e6)
	if stepErr == nil || strings.HasPrefix(stepErr.Error(), "exit code") {
		stepErr = agentError(prof.Name, out, stepErr)
	}

	var outputs map[string]any
	if stepErr == nil {
		outputs, stepErr = rx.captureOutputs(context.WithoutCancel(ctx), st, row.ContainerID, r.dir, lc)
		if stepErr == nil {
			outputs[workflow.ResultOutput] = rx.redact.RedactString(out.result)
		}
	}
	cleanup()
	return outputs, stepErr
}

// agentError maps the stream's ending and the exit to the attempt's error:
// a finished turn with exit 0 succeeds; an expired login fails without a
// retry; anything else fails the attempt and is retried under `retry`.
func agentError(profile string, out agentOutcome, exitErr error) error {
	t := out.terminal
	switch {
	case t != nil && t.Kind == stream.KindAuthRequired:
		return fmt.Errorf("%w: Sign in required: the agent's login for profile %s expired or is missing; run `icaro login %s`", errNoRetry, profile, profile)
	case t != nil && t.Kind == stream.KindRateLimited:
		msg := "the provider's usage limit was reached"
		if t.ResetAt != nil {
			msg += "; it resets at " + t.ResetAt.UTC().Format(time.RFC3339)
		}
		return errors.New(msg)
	case t != nil && t.Kind == stream.KindError:
		return fmt.Errorf("the agent failed: %s", t.Text)
	case t != nil && t.Kind == stream.KindDone && exitErr == nil:
		return nil
	case t != nil && t.Kind == stream.KindDone:
		return fmt.Errorf("the agent finished but exited with %v", exitErr)
	case exitErr != nil:
		return fmt.Errorf("the agent exited without finishing (%v)", exitErr)
	}
	return errors.New("the agent exited without finishing its task")
}

// readAgentStream parses the container's stdout as the CLI's event stream
// and writes it to the step log as readable lines; stderr is kept (capped)
// and appended when the stream ends, so the log has one writer.
func (rx *runExec) readAgentStream(ctx context.Context, containerID, agent string, lc *logCapture) agentOutcome {
	var out agentOutcome
	logs, err := rx.e.d.Docker.Logs(ctx, containerID, true)
	if err != nil {
		lc.line("error: read logs: " + err.Error())
		return out
	}
	defer func() { _ = logs.Close() }()
	pr, pw := io.Pipe()
	stderr := &cappedBuffer{limit: maxStderr}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, err := stdcopy.StdCopy(pw, stderr, logs)
		_ = pw.CloseWithError(err)
	}()
	p, err := stream.New(agent)
	if err != nil {
		lc.line("error: " + err.Error())
		_ = pr.CloseWithError(err)
		<-copied
		return out
	}
	var result strings.Builder
	sum, _ := stream.Read(pr, p, func(ev stream.Event) {
		switch ev.Kind {
		case stream.KindText:
			if result.Len() < maxResult {
				result.WriteString(ev.Text)
			}
		case stream.KindToolCall, stream.KindToolResult:
			// The result is the agent's last message: text after its last
			// tool use.
			result.Reset()
		case stream.KindUsage:
			if u := ev.Usage; u != nil {
				out.usage.Input += u.Input
				out.usage.Output += u.Output
				out.usage.CacheRead += u.CacheRead
				out.usage.CacheWrite += u.CacheWrite
				out.usage.CostUSD += u.CostUSD
			}
		}
		for _, l := range formatEvent(ev) {
			lc.write(true, []byte(l+"\n"))
		}
	})
	// Drain what follows the terminal event so the copier finishes.
	_, _ = io.Copy(io.Discard, pr)
	<-copied
	if stderr.Len() > 0 {
		for _, l := range strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n") {
			lc.write(false, []byte("stderr: "+l+"\n"))
		}
		if stderr.over {
			lc.line("stderr: (truncated)")
		}
	}
	out.terminal = sum.Terminal
	out.result = strings.TrimSpace(result.String())
	if len(out.result) > maxResult {
		out.result = out.result[:maxResult]
	}
	return out
}

// formatEvent renders one stream event as log lines.
func formatEvent(ev stream.Event) []string {
	clip := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > n {
			return s[:n] + "…"
		}
		return s
	}
	name := ev.Tool
	if ev.Server != "" {
		name = ev.Server + "." + ev.Tool
	}
	switch ev.Kind {
	case stream.KindText:
		return strings.Split(strings.TrimRight(ev.Text, "\n"), "\n")
	case stream.KindToolCall:
		return []string{"→ " + name + " " + clip(string(ev.Args), 300)}
	case stream.KindToolResult:
		mark := "←"
		if ev.IsError {
			mark = "← error:"
		}
		return []string{mark + " " + name + " " + clip(ev.Result, 300)}
	case stream.KindUsage:
		u := ev.Usage
		if u == nil {
			return nil
		}
		line := fmt.Sprintf("usage: %d input, %d output tokens", u.Input+u.CacheRead, u.Output)
		if u.CostUSD > 0 {
			line += fmt.Sprintf(", ~$%.4f (the CLI's estimate)", u.CostUSD)
		}
		return []string{line}
	case stream.KindRateLimited:
		return []string{"rate limited: " + ev.Text}
	case stream.KindAuthRequired:
		return []string{"sign in required: " + ev.Text}
	case stream.KindError:
		return []string{"error: " + ev.Text}
	case stream.KindDone:
		return []string{"done"}
	}
	return nil
}

// cappedBuffer keeps the first limit bytes written to it.
type cappedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// promptPath is where an attempt's rendered (redacted) prompt is kept,
// beside its log, so the run shows what the agent was asked.
func (e *Engine) promptPath(runID, stepID string, attempt int) string {
	return filepath.Join(e.cfg.LogsDir, runID, stepID+"."+strconv.Itoa(attempt)+".prompt.md")
}

func (rx *runExec) createAgentContainer(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, r *rendered, as *session.AgentStep, cmd []string, prompt string, attempt int, ioVol string) error {
	e := rx.e
	image := row.ImageDigest
	if err := e.d.Docker.EnsureImage(ctx, image); err != nil {
		return err
	}
	labels := map[string]string{LabelRun: rx.run.ID, LabelStep: st.ID, LabelAttempt: strconv.Itoa(attempt), LabelSession: as.SessionID}
	if ids, err := e.d.Docker.ListByLabel(ctx, map[string]string{LabelRun: rx.run.ID, LabelStep: st.ID, LabelAttempt: strconv.Itoa(attempt)}); err == nil {
		for _, id := range ids {
			_ = e.d.Docker.RemoveContainer(ctx, id)
		}
	}
	if err := e.d.Docker.CreateVolume(ctx, ioVol, labels); err != nil {
		return fmt.Errorf("create io volume: %w", err)
	}
	hc, err := sandbox.HostConfig(sandbox.Options{
		Network: sandbox.NetworkNone, Limits: e.cfg.Limits, Runtime: as.Runtime,
		WorkspaceVolume: "workspace", IOVolume: ioVol,
	})
	if err != nil {
		return err
	}
	for i, m := range hc.Mounts {
		if m.Target == sandbox.WorkspacePath {
			hc.Mounts[i] = mount.Mount{Type: mount.TypeBind, Source: r.dir, Target: sandbox.WorkspacePath}
		}
	}
	// The home is the step session's own layer over the profile's seed.
	switch {
	case as.Home.Volume != "":
		hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.TypeVolume, Source: as.Home.Volume, Target: icontainer.HomePath})
	case as.Home.Dir != "":
		hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.TypeBind, Source: as.Home.Dir, Target: icontainer.HomePath})
	}
	if as.MCPSocket != "" {
		hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.TypeBind, Source: as.MCPSocket, Target: creds.MCPSocket})
	}
	// The agent profile's egress, never the run's workflows.profile.
	if as.Egress.Network != "" {
		hc.NetworkMode = container.NetworkMode(as.Egress.Network)
		if as.Runtime != "" {
			hc.ExtraHosts = as.Egress.Hosts
		}
	}
	runEgress := map[string]bool{}
	for _, kv := range rx.attach.Env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			runEgress[k] = true
		}
	}
	var envs []string
	for _, kv := range r.env {
		k, _, _ := strings.Cut(kv, "=")
		if !runEgress[k] && k != "HOME" {
			envs = append(envs, kv)
		}
	}
	envs = append(envs, "HOME="+icontainer.HomePath, "LANG=C.UTF-8")
	envs = append(envs, as.Egress.Env...)
	envs = append(envs, as.Env...)
	spec := docker.Spec{
		Name: containerName(rx.run.ID, st.ID, attempt), Image: image, Env: envs, WorkingDir: sandbox.WorkspacePath,
		User: e.cfg.User, Labels: labels, HostConfig: hc,
		// The prompt goes in on stdin from the I/O volume, never on the
		// command line.
		Entrypoint: []string{"/bin/sh", "-c", `exec "$@" < ` + sandbox.IOPath + "/" + promptFile, "agent"},
		Cmd:        cmd,
	}
	id, err := e.d.Docker.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.SessionID = store.StepRunning, attempt, id, as.SessionID
	row.StartedAt, row.FinishedAt, row.ExitCode = &now, nil, nil
	row.LogPath, row.LogSize, row.LogLines, row.OutputsJSON = e.logPath(rx.run.ID, st.ID, attempt), 0, 0, "{}"
	if err := e.d.Store.UpdateStepRun(ctx, row); err != nil {
		return err
	}
	e.publish(events.Event{Type: events.TypeStep, RunID: rx.run.ID, Step: row.StepID, Attempt: attempt, Status: row.Status})
	if rx.ws.SessionID != "" {
		if err := e.d.Sessions.SetContainer(ctx, rx.ws.SessionID, id); err != nil {
			e.log.Warn("record session container", "err", err)
		}
	}
	if err := e.d.Sessions.SetContainer(ctx, as.SessionID, id); err != nil {
		e.log.Warn("record step session container", "err", err)
	}
	redacted := rx.redact.RedactString(prompt)
	pp := e.promptPath(rx.run.ID, st.ID, attempt)
	if err := os.MkdirAll(filepath.Dir(pp), 0o700); err == nil {
		_ = os.WriteFile(pp, []byte(redacted), 0o600)
	}
	inputJSON, _ := json.Marshal(r.input)
	uid, gid := parseUser(e.cfg.User)
	files := []tarFile{
		{Name: outputFile, Data: []byte("{}\n"), UID: uid, GID: gid},
		{Name: inputFile, Data: inputJSON, UID: uid, GID: gid},
		{Name: promptFile, Data: []byte(prompt), UID: uid, GID: gid},
	}
	if err := e.d.Docker.CopyIn(ctx, id, sandbox.IOPath, tarball(files)); err != nil {
		return fmt.Errorf("copy inputs: %w", err)
	}
	if err := e.d.Docker.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}
