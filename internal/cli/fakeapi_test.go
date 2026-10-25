package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icaro/internal/apiclient"
	"icaro/internal/integration"
)

// fakeAPI serves canned responses, for client paths the real daemon only
// reaches with Docker (a running session, a run streaming logs).
func fakeAPI(t *testing.T, routes map[string]string) *apiclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if strings.HasPrefix(body, "event:") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	daemonEnv(t)
	t.Setenv("ICARO_DAEMON__URL", srv.URL)
	t.Setenv("ICARO_DAEMON__TOKEN", "tok")
	return apiclient.New(srv.URL, "tok")
}

func TestFollowRunPrintsLogLines(t *testing.T) {
	c := fakeAPI(t, map[string]string{
		"GET /api/v1/runs/r1/events": "event: snapshot\ndata: {}\n\n" +
			"event: log\ndata: {\"step\":\"build\",\"line\":\"compiling\"}\n\n" +
			"event: log\ndata: {\"step\":\"test\",\"line\":\"ok\"}\n\n" +
			"event: end\ndata: {\"id\":\"r1\",\"status\":\"succeeded\"}\n\n",
		// A stream that asks for a resync falls back to polling the run.
		"GET /api/v1/runs/r2/events": "event: resync\ndata: {}\n\n",
		"GET /api/v1/runs/r2":        `{"id":"r2","status":"failed"}`,
	})
	var all strings.Builder
	run, err := followRun(context.Background(), c, "r1", "", &all)
	if err != nil || run.Status != "succeeded" || all.String() != "[build] compiling\n[test] ok\n" {
		t.Fatalf("all steps: %v %+v %q", err, run, all.String())
	}
	var one strings.Builder
	if _, err := followRun(context.Background(), c, "r1", "test", &one); err != nil || one.String() != "ok\n" {
		t.Fatalf("one step: %v %q", err, one.String())
	}
	run, err = followRun(context.Background(), c, "r2", "", &one)
	if err != nil || run.Status != "failed" {
		t.Fatalf("resync: %v %+v", err, run)
	}
	if _, err := followRun(context.Background(), c, "nope", "", &one); err == nil {
		t.Fatal("missing run streamed")
	}

	// The same stream through the CLI, which also checks the exit code.
	out, err := runCLI(t, "", "runs", "logs", "r1", "--follow")
	if err != nil || !strings.Contains(out, "[build] compiling") {
		t.Fatalf("runs logs --follow: %v\n%s", err, out)
	}
}

func TestAgentCommandsNeedARunningSession(t *testing.T) {
	fakeAPI(t, map[string]string{
		"POST /api/v1/sessions/ensure": `{"id":"s1","status":"error","error":"docker down"}`,
	})
	t.Chdir(t.TempDir())
	failCLI(t, "session s1 is error docker down", "claude", "--help")
	failCLI(t, "session s1 is error docker down", "sh")
	t.Setenv("ICARO_PROFILE", "plain")
	failCLI(t, "session s1 is error", "codex")
}

func TestAgentCommandRefusesProfileWithoutAgent(t *testing.T) {
	fakeAPI(t, map[string]string{
		"POST /api/v1/sessions/ensure": `{"id":"s1","status":"running"}`,
		"GET /api/v1/sessions/s1/exec": `{"session_id":"s1","container_id":"c1","user":"1000:1000","workdir":"/workspace"}`,
	})
	t.Chdir(t.TempDir())
	failCLI(t, "profile claude has no agent; use `icaro sh`", "claude")
}

func TestUIReportsSocketHint(t *testing.T) {
	fakeAPI(t, map[string]string{})
	failCLI(t, "icaro ui talks to the local daemon socket", "ui", "--print")
}

func TestPrintIntegrationProblems(t *testing.T) {
	var b strings.Builder
	printIntegrationProblems(&b, &integration.Status{
		Warnings:       []string{"pack acme is behind"},
		Errors:         []integration.LoadError{{Pack: "local", File: "x.yaml", Error: "bad", ServingLastGood: true}, {Pack: "local", File: "y.yaml", Error: "worse"}},
		MissingSecrets: []string{"github.webhook_secret"},
	})
	for _, want := range []string{
		"warning: pack acme is behind",
		"error: local/x.yaml: bad (serving the last good version)",
		"error: local/y.yaml: worse\n",
		"missing webhook secret github.webhook_secret (icaro credential add github webhook_secret)",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q:\n%s", want, b.String())
		}
	}
}

func TestReadInputArgFromStdin(t *testing.T) {
	restore := withStdio(t, "version: 1\n")
	b, err := readInputArg("-")
	restore()
	if err != nil || string(b) != "version: 1\n" {
		t.Fatalf("%v %q", err, b)
	}
}

func TestReadSecretAndPasswordPromptWithoutTerminal(t *testing.T) {
	restore := withStdio(t, "s3cret\r\n")
	v, err := readSecret("ignored: ", &strings.Builder{})
	restore()
	if err != nil || v != "s3cret" {
		t.Fatalf("%v %q", err, v)
	}
	// Without a terminal there is nobody to ask: the prompt returns "" so
	// the caller reports that ICARO_PASSWORD is needed.
	restore = withStdio(t, "")
	pw, err := passwordPrompt(&strings.Builder{}, true)()
	restore()
	if err != nil || pw != "" {
		t.Fatalf("%v %q", err, pw)
	}
}

func TestSessionPushCommand(t *testing.T) {
	fakeAPI(t, map[string]string{
		"POST /api/v1/sessions/s1/push": `{"session_id":"s1","remote":"origin","branch":"icaro/x","commit":"0123456789abcdef0123","forced":false}`,
		"POST /api/v1/sessions/s2/push": `{"session_id":"s2","remote":"up","branch":"icaro/y","commit":"abc","forced":true}`,
	})
	wantCLI(t, []string{"session", "push", "s1"}, "pushed icaro/x (0123456789ab) to origin")
	wantCLI(t, []string{"session", "push", "s1", "--json"}, `"branch": "icaro/x"`)
	// --force-with-lease asks first; "no" sends nothing.
	if out, err := runCLI(t, "n\n", "session", "push", "s2", "--remote", "up", "--force-with-lease"); err != nil || !strings.Contains(out, "Overwrite the remote branch of session s2 on up") {
		t.Fatalf("declined: %v\n%s", err, out)
	}
	if out, err := runCLI(t, "y\n", "session", "push", "s2", "--remote", "", "--force-with-lease"); err != nil || !strings.Contains(out, "on origin") || !strings.Contains(out, "force-pushed (with lease) icaro/y (abc) to up") {
		t.Fatalf("confirmed: %v\n%s", err, out)
	}
	wantCLI(t, []string{"session", "push", "s2", "--force-with-lease", "--yes"}, "force-pushed")
	failCLI(t, "not found", "session", "push", "nope")
}
