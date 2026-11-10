package cli

import (
	"context"
	"strings"
	"testing"
)

// clientOnly makes the CLI behave as it does on Windows (ADR 0039).
func clientOnly(t *testing.T) {
	t.Helper()
	old := daemonSupported
	daemonSupported = func() bool { return false }
	t.Cleanup(func() { daemonSupported = old })
	t.Setenv("ICARO_HOME", t.TempDir())
	t.Setenv("ICARO_CONFIG", "")
}

func TestClientOnlyRefusesHostCommands(t *testing.T) {
	clientOnly(t)
	for _, args := range [][]string{
		{"serve"}, {"service", "status"}, {"init"}, {"setup"}, {"migrate"}, {"token", "list"},
		{"doctor"}, {"update", "--check"}, {"usage"}, {"sh"}, {"claude"}, {"codex"},
		{"login", "default"}, {"session", "attach", "x"},
	} {
		cmd := newRootCmd()
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), "on Windows icaro is a client") {
			t.Errorf("icaro %s: err = %v", strings.Join(args, " "), err)
		}
	}
}

func TestClientOnlyKeepsLocalCommands(t *testing.T) {
	clientOnly(t)
	for _, args := range [][]string{{"version"}, {"schema"}} {
		cmd := newRootCmd()
		cmd.SetArgs(args)
		cmd.SetOut(new(strings.Builder))
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Errorf("icaro %s: %v", strings.Join(args, " "), err)
		}
	}
}

func TestClientOnlyNeedsDaemonURL(t *testing.T) {
	clientOnly(t)
	o := &rootOpts{}
	if _, err := o.client(context.Background()); err == nil || !strings.Contains(err.Error(), "set daemon.url and daemon.token") {
		t.Fatalf("client without daemon.url: %v", err)
	}
	t.Setenv("ICARO_DAEMON__URL", "http://127.0.0.1:1")
	t.Setenv("ICARO_DAEMON__TOKEN", "t")
	if c, err := o.client(context.Background()); err != nil || c == nil {
		t.Fatalf("client with daemon.url: %v", err)
	}
}
