// Command spike-io verifies the step I/O contract under the hardened
// sandbox against a real Docker daemon: a read-only container with the
// seccomp profile and all capabilities dropped must be able to read
// /icaro/input.json (copied in before start) and write /icaro/output.json
// (copied out after exit). Run it where Docker is available:
//
//	go run ./hack/spike-io
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"

	"github.com/ggrocco/icaro/internal/runner/docker"
	"github.com/ggrocco/icaro/internal/runner/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "SPIKE FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("SPIKE OK: input copied in, script ran read-only under seccomp/cap-drop, output copied out")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	eng, err := docker.New("")
	if err != nil {
		return err
	}
	defer func() { _ = eng.Close() }()
	if err := eng.Ping(ctx); err != nil {
		return fmt.Errorf("docker not reachable: %w", err)
	}
	const image = "alpine:3.20"
	if err := eng.EnsureImage(ctx, image); err != nil {
		return err
	}
	ws, io_ := "icaro-spike-ws", "icaro-spike-io"
	_ = eng.CreateVolume(ctx, ws, nil)
	_ = eng.CreateVolume(ctx, io_, nil)
	defer func() { _ = eng.RemoveVolume(ctx, ws); _ = eng.RemoveVolume(ctx, io_) }()

	hc, err := sandbox.HostConfig(sandbox.Options{WorkspaceVolume: ws, IOVolume: io_, Limits: sandbox.Limits{MemoryBytes: 256 << 20, NanoCPUs: 1e9}})
	if err != nil {
		return err
	}
	script := `set -e
echo "whoami: $(id)"
echo "input.json: $(cat /icaro/input.json)"
echo "rootfs writable?"; (touch /x 2>&1 && echo "YES (BAD)") || echo "no (good)"
echo "workspace writable?"; touch /workspace/ok && echo "yes (good)"
echo "home is tmpfs?"; touch $HOME/.ok 2>/dev/null && echo "yes (good)" || echo "no"
echo "unshare blocked?"; (unshare -r true 2>&1 && echo "NO (BAD)") || echo "yes (good)"
printf '{"answer":42,"who":"%s"}' "$(sed -n 's/.*"who":"\([^"]*\)".*/\1/p' /icaro/input.json)" > /icaro/output.json
`
	id, err := eng.CreateContainer(ctx, docker.Spec{
		Name: "icaro-spike", Image: image, Entrypoint: []string{"sh", "-e", "/icaro/script.sh"},
		Env: []string{"HOME=/home/icaro"}, WorkingDir: "/workspace", HostConfig: hc,
		Labels: map[string]string{"icaro.spike": "1"},
	})
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer func() { _ = eng.RemoveContainer(ctx, id) }()

	files := tarFiles(map[string]string{"input.json": `{"who":"spike"}`, "script.sh": script, "output.json": "{}"})
	if err := eng.CopyIn(ctx, id, sandbox.IOPath, files); err != nil {
		return fmt.Errorf("copy in: %w", err)
	}
	if err := eng.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	logs, err := eng.Logs(ctx, id, true)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, logs)
	_ = logs.Close()
	code, err := eng.Wait(ctx, id)
	fmt.Print(out.String())
	if err != nil || code != 0 {
		return fmt.Errorf("container exit=%d err=%v", code, err)
	}
	rd, err := eng.CopyOut(ctx, id, sandbox.IOPath+"/output.json")
	if err != nil {
		return fmt.Errorf("copy out: %w", err)
	}
	defer func() { _ = rd.Close() }()
	raw, err := untarFirst(rd)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("output.json invalid: %w (%q)", err, raw)
	}
	if obj["who"] != "spike" || obj["answer"] != float64(42) {
		return fmt.Errorf("unexpected output: %v", obj)
	}
	fmt.Printf("output.json: %s\n", raw)
	return nil
}

func tarFiles(files map[string]string) io.Reader {
	var buf bytes.Buffer
	tw := newTarWriter(&buf)
	for name, data := range files {
		tw.add(name, data)
	}
	tw.close()
	return &buf
}
