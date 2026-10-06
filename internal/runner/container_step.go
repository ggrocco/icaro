package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"

	"icaro/internal/runner/docker"
	"icaro/internal/runner/logs"
	"icaro/internal/runner/sandbox"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// Labels set on every step container.
const (
	LabelRun     = "icaro.run"
	LabelStep    = "icaro.step"
	LabelAttempt = "icaro.attempt"
	LabelRunner  = "icaro.runner"
)

const (
	inputFile   = "input.json"
	contextFile = "context.json"
	outputFile  = "output.json"
	scriptFile  = "script.sh"
)

// runContainerStep executes one attempt of a `run` step. When resuming it
// re-attaches to the container recorded in row instead of creating one.
func (r *Runner) runContainerStep(ctx context.Context, rc *runCtx, step *workflow.Step, row *store.RunStep, attempt int, resuming bool) error {
	data := rc.templateData(attempt)
	spec := step.Run

	// Resolve templates and secrets up front so failures are cheap.
	script, err := renderOpt("run.script", spec.Script, data)
	if err != nil {
		return err
	}
	command := make([]string, len(spec.Command))
	for i, c := range spec.Command {
		if command[i], err = renderOpt(fmt.Sprintf("run.command[%d]", i), c, data); err != nil {
			return err
		}
	}
	env, secrets, err := r.buildEnv(ctx, rc, step, data)
	if err != nil {
		return err
	}

	containerID := row.ContainerID
	if resuming {
		st, err := r.docker.Inspect(ctx, containerID)
		if err != nil {
			return err
		}
		if !st.Exists {
			return errors.New("container disappeared while the runner was down")
		}
		r.log.Info("re-attached to container", "run", rc.run.ID, "step", step.Name, "container", containerID, "running", st.Running)
	} else {
		if err := r.docker.EnsureImage(ctx, spec.Image); err != nil {
			return err
		}
		limits := sandbox.Limits{
			MemoryBytes: r.cfg.MemoryBytes,
			NanoCPUs:    r.cfg.NanoCPUs,
			Pids:        r.cfg.Pids,
			NoFile:      r.cfg.NoFile,
		}
		if spec.Limits != nil {
			limits.MemoryBytes = sandbox.Tighten(limits.MemoryBytes, spec.Limits.MemoryMB<<20)
			limits.NanoCPUs = sandbox.Tighten(limits.NanoCPUs, int64(spec.Limits.CPUs*1e9))
			limits.Pids = sandbox.Tighten(limits.Pids, spec.Limits.Pids)
		}
		hc, err := sandbox.HostConfig(sandbox.Options{
			Profile: step.Sandbox, Network: step.Network, Limits: limits,
			WorkspaceVolume: rc.wsVol, IOVolume: rc.ioVol, StrictRuntime: r.cfg.StrictRuntime,
		})
		if err != nil {
			return err
		}
		if step.Sandbox == sandbox.Strict && step.Network == "" {
			hc.NetworkMode = "none"
		}

		cspec := docker.Spec{
			Name:       fmt.Sprintf("icaro-%s-%d-%d", strings.ToLower(rc.run.ID), row.Idx, attempt),
			Image:      spec.Image,
			Env:        env,
			WorkingDir: spec.WorkDir,
			HostConfig: hc,
			Labels: map[string]string{
				LabelRun: rc.run.ID, LabelStep: fmt.Sprint(row.Idx), LabelAttempt: fmt.Sprint(attempt), LabelRunner: r.cfg.RunnerID,
			},
		}
		if cspec.WorkingDir == "" {
			cspec.WorkingDir = sandbox.WorkspacePath
		}
		files := []tarFile{{Name: outputFile, Data: []byte("{}\n")}}
		if script != "" {
			shell := spec.Shell
			if shell == "" {
				shell = "sh -e"
			}
			cspec.Entrypoint = append(strings.Fields(shell), sandbox.IOPath+"/"+scriptFile)
			files = append(files, tarFile{Name: scriptFile, Mode: 0o755, Data: []byte(script)})
		} else {
			cspec.Entrypoint = command
		}
		inputJSON, _ := json.Marshal(rc.inputs)
		contextJSON, _ := json.Marshal(data)
		files = append(files, tarFile{Name: inputFile, Data: inputJSON}, tarFile{Name: contextFile, Data: contextJSON})

		containerID, err = r.docker.CreateContainer(ctx, cspec)
		if err != nil {
			return fmt.Errorf("create container: %w", err)
		}
		// Persist before starting so a crash here can be reconciled.
		now := time.Now()
		row.Status, row.Attempt, row.ContainerID, row.StartedAt, row.FinishedAt, row.Error = store.StepRunning, attempt, containerID, &now, nil, ""
		row.ExitCode, row.OutputJSON, row.LogPath, row.LogSize = nil, "{}", r.logs.Path(rc.run.ID, row.Idx), 0
		if err := r.store.UpdateRunStep(ctx, row); err != nil {
			_ = r.docker.RemoveContainer(context.WithoutCancel(ctx), containerID)
			return err
		}
		r.notify(rc.run.ID)
		if err := r.docker.CopyIn(ctx, containerID, sandbox.IOPath, tarball(files)); err != nil {
			_ = r.docker.RemoveContainer(context.WithoutCancel(ctx), containerID)
			return fmt.Errorf("copy inputs: %w", err)
		}
		if err := r.docker.StartContainer(ctx, containerID); err != nil {
			_ = r.docker.RemoveContainer(context.WithoutCancel(ctx), containerID)
			return fmt.Errorf("start container: %w", err)
		}
	}
	defer func() {
		_ = r.docker.RemoveContainer(context.WithoutCancel(ctx), containerID)
	}()

	// Stream logs through the scrubber to disk. On resume the file is
	// rewritten from the beginning so the log is complete without gaps.
	logFile, err := r.logs.Open(rc.run.ID, row.Idx)
	if err != nil {
		return err
	}
	if resuming {
		_ = logFile.Truncate(0)
	}
	scrub := logs.NewScrubber(logFile, secrets)
	logDone := make(chan error, 1)
	go func() {
		defer close(logDone)
		stream, err := r.docker.Logs(context.WithoutCancel(ctx), containerID, true)
		if err != nil {
			logDone <- err
			return
		}
		defer func() { _ = stream.Close() }()
		_, err = stdcopy.StdCopy(scrub, scrub, stream)
		logDone <- err
	}()

	timeout := r.stepTimeout(step)
	wctx, wcancel := context.WithTimeout(ctx, timeout)
	defer wcancel()
	exitCode, werr := r.docker.Wait(wctx, containerID)
	var stepErr error
	switch {
	case werr == nil:
		if exitCode != 0 {
			stepErr = fmt.Errorf("exit code %d", exitCode)
		}
	case ctx.Err() != nil:
		_ = r.docker.Kill(context.WithoutCancel(ctx), containerID)
		stepErr = errCancelled
	case errors.Is(werr, context.DeadlineExceeded):
		_ = r.docker.Kill(context.WithoutCancel(ctx), containerID)
		exitCode = -1
		stepErr = fmt.Errorf("timed out after %s", timeout)
	default:
		stepErr = fmt.Errorf("wait: %w", werr)
	}

	// Drain logs (bounded) and finalize the file.
	select {
	case <-logDone:
	case <-time.After(10 * time.Second):
	}
	_ = scrub.Flush()
	if st, err := logFile.Stat(); err == nil {
		row.LogSize = st.Size()
	}
	_ = logFile.Close()

	// Collect outputs even on failure; they can carry diagnostics.
	outputs, oerr := r.readOutput(context.WithoutCancel(ctx), containerID)
	if oerr != nil && stepErr == nil {
		stepErr = oerr
	}
	now := time.Now()
	row.Attempt, row.ExitCode, row.OutputJSON, row.FinishedAt = attempt, &exitCode, outputs, &now
	if stepErr != nil {
		// The caller decides whether to retry; keep the row as the attempt result.
		row.Status, row.Error = store.StepFailed, stepErr.Error()
		_ = r.store.UpdateRunStep(context.WithoutCancel(ctx), row)
		return stepErr
	}
	row.Status, row.Error = store.StepSucceeded, ""
	return r.store.UpdateRunStep(context.WithoutCancel(ctx), row)
}

// readOutput fetches /icaro/output.json and validates it is a JSON object.
func (r *Runner) readOutput(ctx context.Context, containerID string) (string, error) {
	rd, err := r.docker.CopyOut(ctx, containerID, sandbox.IOPath+"/"+outputFile)
	if errors.Is(err, docker.ErrNotFound) {
		return "{}", nil
	}
	if err != nil {
		return "{}", fmt.Errorf("read output: %w", err)
	}
	defer func() { _ = rd.Close() }()
	raw, err := untarFirst(rd, r.cfg.OutputLimit)
	if errors.Is(err, errTooLarge) || int64(len(raw)) > r.cfg.OutputLimit {
		return "{}", fmt.Errorf("output.json exceeds %d bytes", r.cfg.OutputLimit)
	}
	if err != nil {
		return "{}", fmt.Errorf("read output: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "{}", nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "{}", fmt.Errorf("output.json is not a JSON object: %v", err)
	}
	compact, _ := json.Marshal(obj)
	return string(compact), nil
}

// buildEnv assembles the container environment and the secret values that
// must be scrubbed from logs.
func (r *Runner) buildEnv(ctx context.Context, rc *runCtx, step *workflow.Step, data map[string]any) ([]string, []string, error) {
	env := map[string]string{
		"ICARO_RUN_ID":   rc.run.ID,
		"ICARO_WORKFLOW": rc.run.WorkflowName,
		"ICARO_STEP":     step.Name,
		"HOME":           "/home/icaro",
	}
	for k, v := range rc.inputs {
		switch v.(type) {
		case string, float64, bool, int, int64, json.Number:
			env["ICARO_INPUT_"+envKey(k)] = fmt.Sprint(v)
		}
	}
	var secrets []string
	if step.Connection != "" {
		if r.secrets == nil {
			return nil, nil, errors.New("connections are not configured on this runner")
		}
		fields, err := r.secrets.ResolveConnection(ctx, step.Connection)
		if err != nil {
			return nil, nil, fmt.Errorf("connection %q: %w", step.Connection, err)
		}
		for k, v := range fields {
			env["ICARO_CONN_"+envKey(k)] = v
			secrets = append(secrets, v)
		}
	}
	for _, m := range []map[string]string{step.Run.Env, step.Env} {
		for k, v := range m {
			rendered, err := renderOpt("env."+k, v, data)
			if err != nil {
				return nil, nil, err
			}
			env[k] = rendered
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, secrets, nil
}

func envKey(k string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(k) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func renderOpt(name, text string, data map[string]any) (string, error) {
	if !workflow.HasActions(text) {
		return text, nil
	}
	out, err := workflow.RenderString(name, text, data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}
