package cli

import (
	"bytes"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/container"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

func TestParseValue(t *testing.T) {
	for in, want := range map[string]any{
		"3":        float64(3),
		"true":     true,
		`{"a":1}`:  map[string]any{"a": float64(1)},
		`[1]`:      []any{float64(1)},
		`"quoted"`: `"quoted"`, // a JSON string stays the raw text
		"null":     "null",
		"plain":    "plain",
	} {
		if got := parseValue(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseValue(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestPrompt(t *testing.T) {
	trigger := map[string]any{}
	var out bytes.Buffer
	if err := prompt(strings.NewReader("42\n  hello \n"), &out, []string{"n", "s"}, trigger); err != nil {
		t.Fatal(err)
	}
	if trigger["n"] != float64(42) || trigger["s"] != "hello" {
		t.Fatalf("trigger = %#v", trigger)
	}
	if out.String() != "trigger.n: trigger.s: " {
		t.Fatalf("prompts = %q", out.String())
	}
	if err := prompt(strings.NewReader(""), &out, []string{"missing"}, trigger); err == nil || !strings.Contains(err.Error(), "trigger.missing") {
		t.Fatalf("err = %v", err)
	}
}

func TestWritePrefixed(t *testing.T) {
	var out bytes.Buffer
	writePrefixed(&out, "build", []byte("one\ntwo"))
	writePrefixed(&out, "test", nil)
	if want := "[build] one\n[build] two\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestTriggerSummary(t *testing.T) {
	if got := triggerSummary(nil); got != "manual" {
		t.Fatalf("empty = %q", got)
	}
	got := triggerSummary([]workflow.TriggerSpec{
		{Type: workflow.TriggerCron, Cron: "0 * * * *"},
		{Integration: "github", Event: "push"},
		{Type: workflow.TriggerManual},
	})
	if want := "cron(0 * * * *),github:push,manual"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMemoryParseSince(t *testing.T) {
	before := time.Now().Add(-24 * time.Hour)
	got, err := parseSince("24h")
	if err != nil || got.Before(before.Add(-time.Minute)) || got.After(time.Now()) {
		t.Fatalf("24h = %v, %v", got, err)
	}
	got, err = parseSince("2026-01-02T03:04:05Z")
	if err != nil || !got.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("rfc3339 = %v, %v", got, err)
	}
	if _, err := parseSince("yesterday"); err == nil {
		t.Fatal("want an error for an unparsable --since")
	}
}

func TestSmallFormatters(t *testing.T) {
	if sourceWithLine("a.go", 3) != "a.go:3" || sourceWithLine("a.go", 0) != "a.go" {
		t.Error("sourceWithLine")
	}
	if dash("") != "-" || dash("x") != "x" || intgDash("") != "-" || intgDash("y") != "y" {
		t.Error("dash")
	}
	if intgShort("0123456789abcdef") != "0123456789ab" || intgShort("short") != "short" {
		t.Error("intgShort")
	}
	if intgFirst("", "b", "c") != "b" || intgFirst("", "") != "" {
		t.Error("intgFirst")
	}
	if got := intgSorted([]string{"b", "a"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("intgSorted = %v", got)
	}
	if orDot("") != "." || orDot("x") != "x" {
		t.Error("orDot")
	}
	if !containsSep("a"+string(filepath.Separator)+"b") || containsSep("ab") {
		t.Error("containsSep")
	}
}

func TestPreviewPath(t *testing.T) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := previewPath(nil); err != nil || got != cwd {
		t.Fatalf("default = %q, %v", got, err)
	}
	if got, err := previewPath([]string{"sub"}); err != nil || got != filepath.Join(cwd, "sub") {
		t.Fatalf("relative = %q, %v", got, err)
	}
}

func TestPrintPreview(t *testing.T) {
	var out bytes.Buffer
	printPreview(&out, &apiclient.Preview{
		PreviewInstance: store.PreviewInstance{Status: "running", WorktreePath: "/w", URL: "http://x.localhost",
			Service: "web", Port: 8080, ComposeProject: "p", Error: "boom"},
		Curl: "curl http://x.localhost",
	})
	for _, want := range []string{"running  /w", "url:  http://x.localhost", "curl: curl http://x.localhost",
		"service web port 8080, compose project p", "error: boom"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
	out.Reset()
	printPreview(&out, &apiclient.Preview{PreviewInstance: store.PreviewInstance{Status: "pending", WorktreePath: "/w"}})
	if out.String() != "pending  /w\n" {
		t.Fatalf("minimal = %q", out.String())
	}
}

func TestPrintSessions(t *testing.T) {
	var out bytes.Buffer
	err := printSessions(&out, []*session.Session{
		{ID: "s1", Kind: "agent", Profile: "p", Status: session.StatusRunning, Error: "ignored while running", WorktreePath: "/w1"},
		{ID: "s2", Kind: "run", Profile: "p", Status: "orphaned", Error: "container gone", Branch: "b", WorktreePath: "/w2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(lines[1], "ignored") || !strings.Contains(lines[1], " - ") {
		t.Errorf("running row: %q", lines[1])
	}
	if !strings.Contains(lines[2], "orphaned (container gone)") || !strings.Contains(lines[2], " b ") {
		t.Errorf("orphaned row: %q", lines[2])
	}
}

// The login container gets the same baseline as agent containers.
func TestLoginArgs(t *testing.T) {
	info := &apiclient.LoginInfo{
		Image: "img", SeedDir: "/seed", User: "1000:1000", Command: []string{"claude", "login"},
		Egress: container.Attachment{Network: "icaro-egress-x", Env: []string{"HTTPS_PROXY=http://p:3128"}},
	}
	args := loginArgs(info, "/s.json", true)
	joined := strings.Join(args, " ")
	for _, want := range []string{"run --rm -i -t", "--user 1000:1000", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--security-opt seccomp=/s.json", "--network icaro-egress-x",
		"-v /seed:" + container.HomePath, "-e HTTPS_PROXY=http://p:3128"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if !slices.Equal(args[len(args)-3:], []string{"img", "claude", "login"}) {
		t.Errorf("image and command must come last: %v", args)
	}
	if slices.Contains(loginArgs(info, "/s.json", false), "-t") {
		t.Error("no -t without a terminal")
	}
}

func TestDaemonArgs(t *testing.T) {
	if got := (&rootOpts{}).daemonArgs(); len(got) != 0 {
		t.Fatalf("defaults forward nothing: %v", got)
	}
	got := (&rootOpts{configPath: "/c.yaml", home: "/h"}).daemonArgs()
	if !slices.Equal(got, []string{"--config", "/c.yaml", "--home", "/h"}) {
		t.Fatalf("got %v", got)
	}
}
