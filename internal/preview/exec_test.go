package preview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDocker is a stand-in docker CLI: it logs its arguments and fails or
// answers according to FAKE_* variables passed through CLI.Env.
const fakeDocker = `#!/bin/sh
echo "$(pwd -P)|$*" >> "$FAKE_LOG"
case "$*" in
*" up "*--wait*)
  if [ -n "$FAKE_NOWAIT" ]; then echo "unknown flag: --wait" >&2; exit 1; fi ;;
*" down "*)
  if [ -n "$FAKE_DOWN_ERR" ]; then echo "$FAKE_DOWN_ERR" >&2; exit 1; fi ;;
*" ps "*)
  if [ -n "$FAKE_PS_ERR" ]; then echo "$FAKE_PS_ERR" >&2; exit 1; fi
  echo c1; echo c2 ;;
*" config "*)
  if [ -n "$FAKE_BIG_ERR" ]; then i=0; while [ $i -lt 300 ]; do printf 'xxxxxxxxxx' >&2; i=$((i+1)); done; printf END >&2; exit 2; fi
  echo '{"services":{"web":{"image":"x"}}}' ;;
*" logs "*)
  echo "log line"; echo "err line" >&2 ;;
esac
`

func fakeCLI(t *testing.T, env ...string) (CLI, func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(fakeDocker), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	cli := CLI{Binary: bin, Env: append([]string{"FAKE_LOG=" + log}, env...)}
	return cli, func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func TestCLIConfigUpLogs(t *testing.T) {
	ctx := context.Background()
	cli, calls := fakeCLI(t)
	wt, err := filepath.EvalSymlinks(t.TempDir()) // the script logs `pwd -P`
	if err != nil {
		t.Fatal(err)
	}
	out, err := cli.Config(ctx, wt, []string{"a.yaml", "b.yaml"})
	if err != nil || !strings.Contains(string(out), `"web"`) {
		t.Fatalf("%s %v", out, err)
	}
	if err := cli.Up(ctx, "proj", wt, []string{"a.yaml"}); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := cli.Logs(ctx, "proj", 0, &sb); err != nil || !strings.Contains(sb.String(), "log line") || !strings.Contains(sb.String(), "err line") {
		t.Fatalf("%q %v", sb.String(), err)
	}
	c := calls()
	if len(c) != 3 {
		t.Fatal(c)
	}
	for _, want := range []string{
		wt + "|compose -f a.yaml -f b.yaml --project-directory " + wt + " config --format json",
		wt + "|compose -p proj -f a.yaml --project-directory " + wt + " up -d --build --remove-orphans --wait --wait-timeout 120",
	} {
		if !strings.Contains(strings.Join(c, "\n"), want) {
			t.Fatalf("missing call %q in\n%s", want, strings.Join(c, "\n"))
		}
	}
	// Logs runs from a scratch directory (never the worktree) with the
	// default tail.
	if strings.HasPrefix(c[2], wt+"|") || !strings.HasSuffix(c[2], "compose -p proj logs --no-color --tail 200") {
		t.Fatal(c[2])
	}
}

func TestCLIUpFallsBackWithoutWait(t *testing.T) {
	cli, calls := fakeCLI(t, "FAKE_NOWAIT=1")
	if err := cli.Up(context.Background(), "proj", t.TempDir(), []string{"a.yaml"}); err != nil {
		t.Fatal(err)
	}
	c := calls()
	if len(c) != 2 || strings.Contains(c[1], "--wait") || !strings.HasSuffix(c[1], "up -d --build --remove-orphans") {
		t.Fatalf("an old compose without --wait must be retried without it: %v", c)
	}
}

func TestCLIDownAndRunning(t *testing.T) {
	ctx := context.Background()
	cli, calls := fakeCLI(t)
	if err := cli.Down(ctx, "proj"); err != nil {
		t.Fatal(err)
	}
	if n, err := cli.Running(ctx, "proj"); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	c := calls()
	if !strings.HasSuffix(c[0], "compose -p proj down -v --remove-orphans --timeout 10") || !strings.HasSuffix(c[1], "compose -p proj ps -q --status running") {
		t.Fatal(c)
	}
	// A project that does not exist is not an error for Down or Running.
	for _, msg := range []string{"no configuration file provided", "project not found"} {
		cli, _ := fakeCLI(t, "FAKE_DOWN_ERR="+msg, "FAKE_PS_ERR="+msg)
		if err := cli.Down(ctx, "proj"); err != nil {
			t.Fatalf("%s: %v", msg, err)
		}
	}
	cli, _ = fakeCLI(t, "FAKE_PS_ERR=no configuration file provided")
	if n, err := cli.Running(ctx, "proj"); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	cli, _ = fakeCLI(t, "FAKE_DOWN_ERR=daemon unreachable", "FAKE_PS_ERR=daemon unreachable")
	// A failed command is a ComposeError, which the API passes on as a 502.
	var ce *ComposeError
	if err := cli.Down(ctx, "proj"); !errors.As(err, &ce) || !strings.Contains(err.Error(), "daemon unreachable") {
		t.Fatal(err)
	}
	if _, err := cli.Running(ctx, "proj"); err == nil || !strings.Contains(err.Error(), "daemon unreachable") {
		t.Fatal(err)
	}
}

func TestCLIErrorKeepsStderrTail(t *testing.T) {
	cli, _ := fakeCLI(t, "FAKE_BIG_ERR=1")
	_, err := cli.Config(context.Background(), t.TempDir(), []string{"a.yaml"})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "docker compose -f a.yaml: exit status 2: ") || !strings.HasSuffix(msg, "END") || len(msg) > 2100 {
		t.Fatalf("error must name the command and keep only the stderr tail (%d bytes): %.120s", len(msg), msg)
	}
}

func TestCLIDefaultBinary(t *testing.T) {
	if got := (CLI{}).cmd(context.Background(), "/", "version").Args; got[0] != "docker" || got[1] != "compose" {
		t.Fatal(got)
	}
}
