package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
	"icaro/internal/expr"
	"icaro/internal/logs"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

// Labels set on every step container.
const (
	LabelRun     = "icaro.run"
	LabelStep    = "icaro.step"
	LabelAttempt = "icaro.attempt"
	LabelSession = "icaro.session"
)

const (
	inputFile  = "input.json"
	outputFile = "output.json"
)

func containerName(runID, stepID string, attempt int) string {
	return fmt.Sprintf("icaro-%s-%s-%d", strings.ToLower(runID), stepID, attempt)
}

func ioVolumeName(runID, stepID string, attempt int) string {
	return fmt.Sprintf("icaro-io-%s-%s-%d", strings.ToLower(runID), stepID, attempt)
}

// rendered is an image step's interpolated configuration.
type rendered struct {
	command string
	env     []string
	input   map[string]any
	dir     string // host directory mounted at /workspace
}

func (rx *runExec) render(ctx context.Context, st *workflow.StepSpec, env expr.Env, attempt int) (*rendered, error) {
	r := &rendered{}
	var err error
	if r.command, err = expr.RenderString(st.Command, env); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	in, err := expr.EvalValue(map[string]any(st.Input), env)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	r.input, _ = in.(map[string]any)
	if r.input == nil {
		r.input = map[string]any{}
	}
	vars := map[string]string{
		"ICARO_RUN_ID":   rx.run.ID,
		"ICARO_WORKFLOW": rx.run.WorkflowName,
		"ICARO_STEP":     st.ID,
		"ICARO_ATTEMPT":  strconv.Itoa(attempt),
		"HOME":           "/home",
	}
	for k, v := range r.input {
		switch v.(type) {
		case string, float64, bool:
			vars["ICARO_INPUT_"+envKey(k)] = expr.Stringify(v)
		}
	}
	for k, v := range rx.env {
		vars[k] = v
	}
	for k, v := range st.Env {
		out, err := expr.RenderString(v, env)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", k, err)
		}
		vars[k] = out
	}
	for _, kv := range rx.attach.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.env = append(r.env, k+"="+vars[k])
	}
	r.dir = rx.ws.Dir
	if st.Repository != nil {
		ref, rev, err := rx.renderRepository(st.Repository, env)
		if err != nil {
			return nil, err
		}
		if r.dir, err = rx.e.d.Sessions.RepoWorktree(ctx, rx.ws.SessionID, ref, rev); err != nil {
			return nil, fmt.Errorf("repository %s: %w", ref, err)
		}
	}
	return r, nil
}

// renderRepository interpolates a step's repository ref and revision and
// checks the result: a trigger payload must not turn either into a git
// option, and a secret must not end up in the stored ref.
func (rx *runExec) renderRepository(repo *workflow.RepositoryRef, env expr.Env) (ref, rev string, err error) {
	if ref, err = expr.RenderString(repo.Ref, env); err != nil {
		return "", "", fmt.Errorf("repository.ref: %w", err)
	}
	if rev, err = expr.RenderString(repo.Revision, env); err != nil {
		return "", "", fmt.Errorf("repository.revision: %w", err)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", errors.New("repository.ref is empty")
	}
	for _, f := range []struct{ name, val string }{{"ref", ref}, {"revision", rev}} {
		if rx.redact.RedactString(f.val) != f.val {
			return "", "", fmt.Errorf("repository.%s contains a secret value", f.name)
		}
		if err := workflow.CheckRepoValue(f.val); err != nil {
			return "", "", fmt.Errorf("repository.%s: %w", f.name, err)
		}
	}
	return ref, rev, nil
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

// imageAttempt runs (or, with reattach, resumes) one container attempt.
func (rx *runExec) imageAttempt(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, env expr.Env, attempt int, reattach bool) (map[string]any, error) {
	e := rx.e
	r, err := rx.render(ctx, st, env, attempt)
	if err != nil {
		row.Attempt = attempt
		return nil, err
	}
	ioVol := ioVolumeName(rx.run.ID, st.ID, attempt)
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if row.ContainerID != "" {
			_ = e.d.Docker.RemoveContainer(cctx, row.ContainerID)
		}
		_ = e.d.Docker.RemoveVolume(cctx, ioVol)
	}

	if reattach {
		exists, started, err := e.d.Docker.ContainerStarted(ctx, row.ContainerID)
		if err != nil {
			return nil, err
		}
		if !exists || !started {
			cleanup()
			if !exists {
				return nil, errors.New("container disappeared while the daemon was down")
			}
			return nil, errors.New("the daemon stopped before the container started")
		}
		e.log.Info("re-attached to step container", "run", rx.run.ID, "step", st.ID, "container", row.ContainerID)
	} else {
		if err := rx.createContainer(ctx, st, row, r, attempt, ioVol); err != nil {
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
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		stream, err := e.d.Docker.Logs(streamCtx, row.ContainerID, true)
		if err != nil {
			lc.line("error: read logs: " + err.Error())
			return
		}
		defer func() { _ = stream.Close() }()
		_, _ = stdcopy.StdCopy(lc.writer(true), lc.writer(false), stream)
	}()
	finishLogs := func(wait time.Duration) {
		select {
		case <-logDone:
		case <-time.After(wait):
		}
		stopStream()
		<-logDone
		lc.close()
	}

	timeout := e.stepTimeout(st)
	wctx, wcancel := context.WithTimeout(ctx, timeout)
	defer wcancel()
	exitCode, werr := e.d.Docker.Wait(wctx, row.ContainerID)
	var stepErr error
	switch {
	case werr == nil:
		if exitCode != 0 {
			stepErr = fmt.Errorf("exit code %d", exitCode)
		}
	case ctx.Err() != nil && !cancelled(ctx):
		// Daemon stopping: leave the container running for re-attach.
		finishLogs(0)
		return nil, errShutdown
	case ctx.Err() != nil:
		_ = e.d.Docker.Kill(context.WithoutCancel(ctx), row.ContainerID)
		exitCode, stepErr = 137, errCancelled
	case errors.Is(werr, context.DeadlineExceeded):
		_ = e.d.Docker.Kill(context.WithoutCancel(ctx), row.ContainerID)
		exitCode, stepErr = -1, fmt.Errorf("timed out after %s", timeout)
	default:
		stepErr = fmt.Errorf("wait: %w", werr)
	}
	finishLogs(10 * time.Second)
	row.ExitCode = &exitCode
	row.LogSize, row.LogLines = lc.offset, lc.lines

	var outputs map[string]any
	if stepErr == nil {
		outputs, stepErr = rx.captureOutputs(context.WithoutCancel(ctx), st, row.ContainerID, r.dir, lc)
	}
	cleanup()
	return outputs, stepErr
}

func (rx *runExec) createContainer(ctx context.Context, st *workflow.StepSpec, row *store.StepRun, r *rendered, attempt int, ioVol string) error {
	e := rx.e
	if err := e.d.Docker.EnsureImage(ctx, st.Image); err != nil {
		return err
	}
	labels := map[string]string{LabelRun: rx.run.ID, LabelStep: st.ID, LabelAttempt: strconv.Itoa(attempt), LabelSession: rx.ws.SessionID}
	// Remove leftovers of this exact attempt (a crash between create and
	// persist leaves a labeled container no row points at).
	if ids, err := e.d.Docker.ListByLabel(ctx, map[string]string{LabelRun: rx.run.ID, LabelStep: st.ID, LabelAttempt: strconv.Itoa(attempt)}); err == nil {
		for _, id := range ids {
			_ = e.d.Docker.RemoveContainer(ctx, id)
		}
	}
	if err := e.d.Docker.CreateVolume(ctx, ioVol, labels); err != nil {
		return fmt.Errorf("create io volume: %w", err)
	}
	hc, err := sandbox.HostConfig(sandbox.Options{
		Network: sandbox.NetworkNone, Limits: e.cfg.Limits, Runtime: e.cfg.Runtime,
		WorkspaceVolume: "workspace", IOVolume: ioVol,
	})
	if err != nil {
		return err
	}
	// The workspace is the run session's directory, bind-mounted (ADR 0010).
	for i, m := range hc.Mounts {
		if m.Target == sandbox.WorkspacePath {
			hc.Mounts[i] = mount.Mount{Type: mount.TypeBind, Source: r.dir, Target: sandbox.WorkspacePath}
		}
	}
	if rx.attach.Network != "" {
		hc.NetworkMode = container.NetworkMode(rx.attach.Network)
		if e.cfg.Runtime != "" {
			// Another runtime (gVisor) cannot use Docker's DNS: name the
			// proxy in /etc/hosts.
			hc.ExtraHosts = rx.attach.Hosts
		}
	}
	spec := docker.Spec{
		Name: containerName(rx.run.ID, st.ID, attempt), Image: st.Image, Env: r.env, WorkingDir: sandbox.WorkspacePath,
		User: e.cfg.User, Labels: labels, HostConfig: hc,
	}
	if r.command != "" {
		spec.Entrypoint, spec.Cmd = []string{"/bin/sh", "-c"}, []string{r.command}
	}
	id, err := e.d.Docker.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	// Persist the container id before starting, so a crash here is
	// reconciled by re-attaching instead of starting a duplicate.
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.SessionID = store.StepRunning, attempt, id, rx.ws.SessionID
	row.StartedAt, row.FinishedAt, row.ExitCode = &now, nil, nil
	row.LogPath, row.LogSize, row.LogLines, row.OutputsJSON = e.logPath(rx.run.ID, st.ID, attempt), 0, 0, "{}"
	if err := e.d.Store.UpdateStepRun(ctx, row); err != nil {
		return err
	}
	rx.e.publish(events.Event{Type: events.TypeStep, RunID: rx.run.ID, Step: row.StepID, Attempt: attempt, Status: row.Status})
	if rx.ws.SessionID != "" {
		if err := e.d.Sessions.SetContainer(ctx, rx.ws.SessionID, id); err != nil {
			e.log.Warn("record session container", "err", err)
		}
	}
	inputJSON, _ := json.Marshal(r.input)
	uid, gid := parseUser(e.cfg.User)
	files := []tarFile{{Name: outputFile, Data: []byte("{}\n"), UID: uid, GID: gid}, {Name: inputFile, Data: inputJSON, UID: uid, GID: gid}}
	if err := e.d.Docker.CopyIn(ctx, id, sandbox.IOPath, tarball(files)); err != nil {
		return fmt.Errorf("copy inputs: %w", err)
	}
	if err := e.d.Docker.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

// captureOutputs evaluates the step's declared outputs (ADR 0013).
func (rx *runExec) captureOutputs(ctx context.Context, st *workflow.StepSpec, containerID, dir string, lc *logCapture) (map[string]any, error) {
	outputs := map[string]any{}
	if len(st.Outputs) == 0 {
		// Still enforce the output.json contract (an object, <= 1 MiB).
		_, err := rx.readOutputJSON(ctx, containerID)
		return outputs, err
	}
	var doc map[string]any
	docRead := false
	for _, key := range sortStrings(mapKeys(st.Outputs)) {
		c, err := workflow.ParseCapture(st.Outputs[key])
		if err != nil {
			return nil, fmt.Errorf("output %s: %w", key, err)
		}
		switch c.Kind {
		case workflow.CaptureJSON:
			if !docRead {
				if doc, err = rx.readOutputJSON(ctx, containerID); err != nil {
					return nil, err
				}
				docRead = true
			}
			v, err := c.FromJSON(doc)
			if err != nil {
				return nil, fmt.Errorf("output %s: %w", key, err)
			}
			outputs[key] = redactValue(rx.redact, v)
		case workflow.CaptureStdout, workflow.CaptureStdoutLast:
			if lc.stdoutOver {
				return nil, fmt.Errorf("output %s: stdout exceeds %d bytes", key, rx.e.cfg.OutputLimit)
			}
			s := lc.stdout.String()
			if c.Kind == workflow.CaptureStdoutLast {
				outputs[key] = workflow.LastLine(s)
			} else {
				outputs[key] = strings.TrimSpace(s)
			}
		case workflow.CaptureFile:
			v, err := rx.readWorkspaceFile(dir, c.File)
			if err != nil {
				return nil, fmt.Errorf("output %s: %w", key, err)
			}
			outputs[key] = v
		}
	}
	return outputs, nil
}

func (rx *runExec) readOutputJSON(ctx context.Context, containerID string) (map[string]any, error) {
	limit := rx.e.cfg.OutputLimit
	rd, err := rx.e.d.Docker.CopyOut(ctx, containerID, sandbox.IOPath+"/"+outputFile)
	if errors.Is(err, docker.ErrNotFound) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read output.json: %w", err)
	}
	defer func() { _ = rd.Close() }()
	raw, err := untarFirst(rd, limit)
	if errors.Is(err, errTooLarge) || int64(len(raw)) > limit {
		return nil, fmt.Errorf("output.json exceeds %d bytes", limit)
	}
	if err != nil {
		return nil, fmt.Errorf("read output.json: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("output.json is not a JSON object: %v", err)
	}
	return obj, nil
}

// readWorkspaceFile reads a capture file, refusing paths that escape the
// workspace through symlinks.
func (rx *runExec) readWorkspaceFile(dir, rel string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("file %s: %w", rel, err)
	}
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("file %s resolves outside /workspace", rel)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if fi.Size() > rx.e.cfg.OutputLimit {
		return "", fmt.Errorf("file %s exceeds %d bytes", rel, rx.e.cfg.OutputLimit)
	}
	b := make([]byte, fi.Size())
	if _, err := f.ReadAt(b, 0); err != nil && fi.Size() > 0 {
		return "", err
	}
	return rx.redact.RedactString(strings.TrimSpace(string(b))), nil
}

// redactValue applies the run's redactor to every string inside a
// JSON value, object keys included: captured values pass through the same
// redactor as logs (ADR 0013), however deep the secret is nested.
func redactValue(r *logs.Redactor, v any) any {
	switch x := v.(type) {
	case string:
		return r.RedactString(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[r.RedactString(k)] = redactValue(r, val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactValue(r, val)
		}
		return out
	}
	return v
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortStrings(s []string) []string {
	sort.Strings(s)
	return s
}

// parseUser reads "uid:gid" (or "uid"); anything else owns files as root.
func parseUser(u string) (int, int) {
	uidS, gidS, _ := strings.Cut(u, ":")
	uid, err := strconv.Atoi(uidS)
	if err != nil {
		return 0, 0
	}
	gid, err := strconv.Atoi(gidS)
	if err != nil {
		gid = uid
	}
	return uid, gid
}
