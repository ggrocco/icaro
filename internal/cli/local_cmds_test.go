package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"icaro/internal/daemon"
	"icaro/internal/update"
)

func TestTokenInitMigrateAndConfig(t *testing.T) {
	daemonEnv(t)

	wantCLI(t, []string{"init"}, "master key: ", "initialized ", "next: icaro setup")
	mustCLI(t, "migrate")

	tok := strings.TrimSpace(mustCLI(t, "token", "create", "--name", "ci", "--scope", "read"))
	if tok == "" {
		t.Fatal("no token printed")
	}
	failCLI(t, `token "ci" already exists`, "token", "create", "--name", "ci")
	failCLI(t, "scope must be read, write or admin", "token", "create", "--name", "x", "--scope", "root")
	failCLI(t, `required flag(s) "name"`, "token", "create")
	wantCLI(t, []string{"token", "list"}, "NAME", "ci", "read", "-")
	mustCLI(t, "token", "revoke", "ci")
	out := mustCLI(t, "token", "list")
	if strings.Count(strings.Split(out, "\n")[1], "-") == 0 || strings.HasSuffix(strings.TrimSpace(strings.Split(out, "\n")[1]), " -") {
		t.Errorf("revoked token shows no revocation time:\n%s", out)
	}
	failCLI(t, `no active token named "ci"`, "token", "revoke", "ci")

	// config show redacts the API token.
	t.Setenv("ICARO_DAEMON__TOKEN", "super-secret")
	out = wantCLI(t, []string{"config", "show"}, "version: 1", "token: '***'")
	if strings.Contains(out, "super-secret") {
		t.Fatalf("token leaked:\n%s", out)
	}

	// An invalid configuration fails every command that loads it.
	t.Setenv("ICARO_DAEMON__UI_LISTEN", "0.0.0.0:8787")
	failCLI(t, "must be a loopback address", "config", "show")
	failCLI(t, "must be a loopback address", "migrate")
	failCLI(t, "must be a loopback address", "token", "list")
	failCLI(t, "must be a loopback address", "token", "revoke", "x")
	failCLI(t, "must be a loopback address", "token", "create", "--name", "y")
	failCLI(t, "must be a loopback address", "init")
	failCLI(t, "must be a loopback address", "workflow", "list")
	failCLI(t, "must be a loopback address", "usage")
	failCLI(t, "must be a loopback address", "update", "--check")
	failCLI(t, "must be a loopback address", "service", "status")
	failCLI(t, "must be a loopback address", "preview", "lint", t.TempDir())
	failCLI(t, "must be a loopback address", "doctor")
	failCLI(t, "must be a loopback address", "mcp")
	failCLI(t, "must be a loopback address", "integration", "add", "file:///nowhere")
	failCLI(t, "must be a loopback address", "integration", "update", "x")
	failCLI(t, "must be a loopback address", "integration", "push", "x")
	failCLI(t, "must be a loopback address", "integration", "remove", "x", "--yes")
	failCLI(t, "must be a loopback address", "serve")
}

func TestRemoteDaemonNeedsToken(t *testing.T) {
	daemonEnv(t)
	t.Setenv("ICARO_DAEMON__URL", "http://127.0.0.1:1")
	failCLI(t, "daemon.url is set but no token", "workflow", "list")
	failCLI(t, "daemon.url is set but no token", "mcp")
}

func TestSecretCommands(t *testing.T) {
	startDaemon(t)

	restore := withStdio(t, "hunter2\n")
	out, err := runCLI(t, "", "secret", "set", "API_KEY", "--host", "api.example.com")
	restore()
	if err != nil || !strings.Contains(out, "stored API_KEY, bound to api.example.com") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	restore = withStdio(t, "plain")
	out, err = runCLI(t, "", "secret", "set", "PLAIN")
	restore()
	if err != nil || !strings.Contains(out, "not bound to any host") {
		t.Fatalf("set unbound: %v\n%s", err, out)
	}
	restore = withStdio(t, "\n")
	_, err = runCLI(t, "", "secret", "set", "EMPTY")
	restore()
	if err == nil || !strings.Contains(err.Error(), "empty value") {
		t.Fatalf("empty secret: %v", err)
	}
	restore = withStdio(t, "v")
	_, err = runCLI(t, "", "secret", "set", "bad name!")
	restore()
	if err == nil {
		t.Fatal("invalid secret name accepted")
	}

	wantCLI(t, []string{"secret", "list"}, "NAME", "API_KEY", "api.example.com", "PLAIN")
	if out, err := runCLI(t, "n\n", "secret", "rm", "API_KEY"); err != nil || !strings.Contains(out, "Delete secret API_KEY?") {
		t.Fatalf("rm declined: %v\n%s", err, out)
	}
	wantCLI(t, []string{"secret", "list"}, "API_KEY")
	if _, err := runCLI(t, "yes\n", "secret", "rm", "API_KEY"); err != nil {
		t.Fatal(err)
	}
	mustCLI(t, "secret", "rm", "PLAIN", "--yes")
	failCLI(t, "", "secret", "rm", "PLAIN", "--yes")
	if out := mustCLI(t, "secret", "list"); strings.Contains(out, "API_KEY") || strings.Contains(out, "PLAIN") {
		t.Fatalf("secrets not removed:\n%s", out)
	}
}

func TestMCPBridge(t *testing.T) {
	home := startDaemon(t)
	repo := projectRepo(t)
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n"

	// Host mode: scoped to --project, through the daemon socket.
	restore := withStdio(t, initialize)
	_, err := runCLI(t, "", "mcp", "--project", repo, "--agent", "claude")
	out := restore()
	if err != nil || !strings.Contains(out, `"id":1`) || !strings.Contains(out, `"result"`) {
		t.Fatalf("host bridge: %v\n%s", err, out)
	}

	// Host mode from inside a repository picks it up without --project.
	t.Chdir(repo)
	restore = withStdio(t, initialize)
	_, err = runCLI(t, "", "mcp")
	if out := restore(); err != nil || !strings.Contains(out, `"result"`) {
		t.Fatalf("bridge from a repo: %v\n%s", err, out)
	}

	// Socket mode, as inside a session container; the daemon socket
	// serves /mcp too.
	restore = withStdio(t, initialize)
	_, err = runCLI(t, "", "mcp", "--socket", filepath.Join(home, "run", "icaro.sock"))
	if out := restore(); err != nil || !strings.Contains(out, `"result"`) {
		t.Fatalf("socket bridge: %v\n%s", err, out)
	}
	failCLI(t, "mcp socket", "mcp", "--socket", filepath.Join(home, "missing.sock"))

	if !isRepo(repo) || isRepo(t.TempDir()) {
		t.Error("isRepo misclassifies directories")
	}
	if orDot("") != "." || orDot("x") != "x" {
		t.Error("orDot")
	}
}

func TestMCPBridgeOverTCPNeedsToken(t *testing.T) {
	daemonEnv(t)
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ICARO_DAEMON__URL", srv.URL)
	t.Setenv("ICARO_DAEMON__TOKEN", "tok")
	restore := withStdio(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
	_, err := runCLI(t, "", "mcp", "--project", t.TempDir())
	out := restore()
	if err != nil || !strings.Contains(out, `"result"`) || auth != "Bearer tok" {
		t.Fatalf("tcp bridge: %v auth=%q\n%s", err, auth, out)
	}
}

func TestUsageCommand(t *testing.T) {
	home := daemonEnv(t)
	dir := filepath.Join(home, "home", "claude", ".claude", "projects", "-workspace")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	line := func(id, model string) string {
		b, _ := json.Marshal(map[string]any{
			"type": "assistant", "timestamp": time.Now().UTC().Format(time.RFC3339), "requestId": "req_" + id,
			"message": map[string]any{"id": "msg_" + id, "model": model, "usage": map[string]any{"input_tokens": 1200, "output_tokens": 300}},
		})
		return string(b)
	}
	writeFile(t, dir, "session.jsonl", line("1", "claude-sonnet-4-20250514")+"\n"+line("2", "claude-future-9")+"\n")
	t.Setenv("ICARO_USAGE__DAILY_LIMIT_USD", "0.0001")

	wantCLI(t, []string{"usage"}, "DAY", "profile:claude", "claude-sonnet-4-20250514", "~$", "n/a", "ALERT ", "(at least; some usage is unpriced)")
	wantCLI(t, []string{"usage", "--json", "--no-save", "--since", "30d"}, `"records"`)
	failCLI(t, "", "usage", "--since", "whenever")

	// --host-claude adds the user's own transcripts; a missing directory
	// is reported as an unreadable source rather than hidden.
	t.Setenv("HOME", t.TempDir())
	wantCLI(t, []string{"usage", "--host-claude", "--no-save"}, "host")

	// Unreadable sources are rows with their error.
	printUsage(&strings.Builder{}, nil)
}

func TestUpdateCommandSafePaths(t *testing.T) {
	daemonEnv(t)
	latest := "v9.9.9"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			_, _ = w.Write([]byte(latest + "\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ICARO_UPDATE__BASE_URL", srv.URL)
	old := version
	version = "v1.0.0"
	t.Cleanup(func() { version = old })

	wantCLI(t, []string{"update", "--check"}, "update available: v1.0.0 -> v9.9.9")
	failCLI(t, "re-run with --yes", "update")
	wantCLI(t, []string{"update", "--check", "--version", "v2.0.0"}, "update available: v1.0.0 -> v2.0.0")
	// A broken release key stops the install before anything is fetched.
	t.Setenv("ICARO_UPDATE__PUBLIC_KEY", "not-a-key")
	failCLI(t, "cannot verify releases", "update", "--yes")
	latest = "v1.0.0"
	wantCLI(t, []string{"update"}, "icaro v1.0.0 is up to date (latest v1.0.0)")

	t.Setenv("ICARO_UPDATE__BASE_URL", srv.URL+"/missing")
	failCLI(t, "", "update", "--check")

	failCLI(t, "re-run with --yes", "update", "--rollback")
	exe, err := update.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(update.PrevPath(exe)); err == nil {
		t.Skip("a previous test binary exists; rollback would replace the running test binary")
	}
	failCLI(t, "nothing to roll back to", "update", "--rollback", "--yes")
}

func TestServiceCommandSafePaths(t *testing.T) {
	daemonEnv(t)
	t.Setenv("HOME", t.TempDir())
	wantCLI(t, []string{"service", "install", "--dry-run"}, "# ", "serve")
	failCLI(t, "re-run with --yes", "service", "uninstall")
	// status only reads: it reports "installed: false" on a fresh home, or
	// fails when the service manager is unavailable (e.g. no systemd).
	if out, err := runCLI(t, "", "service", "status"); err == nil && !strings.Contains(out, "installed: false") {
		t.Fatalf("status on a fresh home:\n%s", out)
	}
}

func TestUICommandPrintsLink(t *testing.T) {
	startDaemon(t)
	out, err := runCLI(t, "", "ui", "--print")
	if err != nil {
		t.Fatalf("ui: %v\n%s", err, out)
	}
	if !strings.Contains(out, "http://127.0.0.1:") || !strings.Contains(out, "single use, valid for") {
		t.Fatalf("ui output:\n%s", out)
	}
}

// TestServeRemoteListen: a non-loopback --listen needs --allow-remote, and
// the daemon warns when it is given.
func TestServeRemoteListen(t *testing.T) {
	daemonEnv(t)
	failCLI(t, "not a loopback address", "serve", "--listen", "0.0.0.0:0")
	t.Setenv("ICARO_DAEMON__LISTEN", ":8787")
	failCLI(t, "daemon.allow_remote", "config", "show")
	t.Setenv("ICARO_DAEMON__ALLOW_REMOTE", "true")
	mustCLI(t, "config", "show")
	t.Setenv("ICARO_DAEMON__ALLOW_REMOTE", "")
	t.Setenv("ICARO_DAEMON__LISTEN", "")

	cfg, err := (&rootOpts{}).loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Daemon.Listen, cfg.Daemon.AllowRemote = "0.0.0.0:0", true
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, cfg, slog.New(slog.NewTextHandler(&logs, nil))) }()
	waitDaemon(t, cfg, done)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "daemon.listen is not loopback") {
		t.Errorf("no warning for a remote listener:\n%s", logs.String())
	}
}

func TestServeCommand(t *testing.T) {
	daemonEnv(t)
	failCLI(t, "invalid --log-level", "serve", "--log-level", "loud")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runCLICtx(ctx, "serve", "--listen", "127.0.0.1:0", "--pprof", "--log-level", "error")
		done <- err
	}()
	cfg := layout(t)
	waitDaemon(t, cfg, done)

	// A second daemon for the same home refuses to start, with exit code 3.
	if code := Main([]string{"serve"}); code != 3 {
		t.Errorf("second serve exit code = %d, want 3", code)
	}
	// pprof is mounted on the socket.
	resp, err := daemon.HTTPClient(cfg.Layout(), 0).Get(daemon.SocketBaseURL + "/debug/pprof/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("pprof: %s", resp.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestEgressProxyCommandServesUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := runCLICtx(ctx, "egress-proxy", "--listen", "127.0.0.1:0", "--allow", "api.anthropic.com, ,*.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLICtx(context.Background(), "egress-proxy", "--listen", "256.0.0.1:99999"); err == nil {
		t.Fatal("bad listen address accepted")
	}
}

func TestMainExitCodes(t *testing.T) {
	daemonEnv(t)
	if code := Main([]string{"version"}); code != 0 {
		t.Errorf("version: %d", code)
	}
	if code := Main([]string{"version", "--long"}); code != 0 {
		t.Errorf("version --long: %d", code)
	}
	if code := Main([]string{"no-such-command"}); code != 1 {
		t.Errorf("unknown command: %d", code)
	}
	if Version() != version || Tree().Name() != "icaro" {
		t.Error("Version/Tree")
	}
	if (&ExitError{Code: 7}).Error() != "exit status 7" {
		t.Error("ExitError text")
	}
}

// A run that does not succeed makes `icaro run --wait` exit 1 through
// ExitError; Main maps it to the process exit code.
func TestRunWaitExitCode(t *testing.T) {
	startDaemon(t)
	mustCLI(t, "workflow", "apply", writeFile(t, t.TempDir(), "hello.yaml", helloYAML))
	code := make(chan int, 1)
	restore := withStdio(t, "")
	go func() { code <- Main([]string{"run", "hello", "--set", "who=x", "--wait"}) }()

	// The run stays queued (no Docker); cancel it so the wait ends.
	var id string
	deadline := time.Now().Add(10 * time.Second)
	for id == "" && time.Now().Before(deadline) {
		var runs []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(mustCLI(t, "runs", "list", "--json")), &runs) == nil && len(runs) > 0 {
			id = runs[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("run never appeared")
	}
	mustCLI(t, "runs", "cancel", id)
	select {
	case c := <-code:
		restore()
		if c != 1 {
			t.Fatalf("run --wait exit code = %d, want 1", c)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run --wait did not return")
	}

	// --json waits silently and prints the final run.
	// --json keeps the exit status: the cancelled run still exits 1.
	restore = withStdio(t, "")
	go func() { code <- Main([]string{"run", "hello", "--set", "who=y", "--wait", "--json"}) }()
	id = ""
	for deadline = time.Now().Add(10 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var runs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(mustCLI(t, "runs", "list", "--json", "--status", "queued")), &runs) == nil && len(runs) > 0 {
			id = runs[0].ID
		}
	}
	if id == "" {
		t.Fatal("second run never appeared")
	}
	mustCLI(t, "runs", "cancel", id)
	select {
	case c := <-code:
		out := restore()
		var final struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &final); err != nil || final.Status != "cancelled" {
			t.Fatalf("run --wait --json must still print the final run: %v\n%s", err, out)
		}
		if c != 1 {
			t.Fatalf("run --wait --json exit code = %d, want 1", c)
		}
	case <-time.After(15 * time.Second):
		restore()
		t.Fatal("run --wait --json did not return")
	}
}

// syncBuffer is a bytes.Buffer safe for a logger in another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
