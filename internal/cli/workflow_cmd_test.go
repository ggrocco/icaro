package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const helloYAML = `version: 1
name: hello
triggers:
  - type: manual
steps:
  - id: greet
    image: mirror.gcr.io/library/alpine:3.24
    env:
      WHO: ${{ trigger.who }}
    command: echo "hello $WHO"
  - id: shout
    image: mirror.gcr.io/library/alpine:3.24
    needs: [greet]
    command: echo HI
`

func TestWorkflowLifecycle(t *testing.T) {
	startDaemon(t)
	dir := t.TempDir()
	file := writeFile(t, dir, "hello.yaml", helloYAML)

	wantCLI(t, []string{"workflow", "validate", file}, "valid: hello")
	out := mustCLI(t, "workflow", "validate", "--json", file)
	if !strings.Contains(out, `"valid": true`) {
		t.Errorf("validate --json: %s", out)
	}
	bad := writeFile(t, dir, "bad.yaml", "version: 1\nname: bad\nsteps:\n  - id: a\n    needs: [missing]\n")
	failCLI(t, "issue(s)", "workflow", "validate", bad)
	// --json prints the issues and still fails.
	out, err := runCLI(t, "", "workflow", "validate", "--json", bad)
	if err == nil || !strings.Contains(out, `"valid": false`) || !strings.Contains(out, `"issues"`) {
		t.Fatalf("validate --json on an invalid file: %v\n%s", err, out)
	}
	failCLI(t, "no such file", "workflow", "validate", dir+"/absent.yaml")
	failCLI(t, "no such file", "workflow", "apply", dir+"/absent.yaml")

	wantCLI(t, []string{"workflow", "apply", file}, "applied hello version 1", "manual runs need: --set who=...")
	// A second apply with a change makes version 2; --json prints the info.
	out = mustCLI(t, "workflow", "apply", "--json", writeFile(t, dir, "v2.yaml", strings.Replace(helloYAML, "echo HI", "echo HO", 1)))
	var info struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil || info.Version != 2 {
		t.Fatalf("apply --json: %v %s", err, out)
	}
	failCLI(t, "", "workflow", "apply", bad)

	wantCLI(t, []string{"workflow", "list"}, "hello", "manual")
	wantCLI(t, []string{"workflow", "list", "--json"}, `"name": "hello"`)
	wantCLI(t, []string{"workflow", "get", "hello"}, "# hello v2", "echo HO")
	wantCLI(t, []string{"workflow", "get", "--json", "hello"}, `"version": 2`)
	failCLI(t, "", "workflow", "get", "nope")

	wantCLI(t, []string{"workflow", "versions", "hello"}, "VERSION", "1", "2")
	wantCLI(t, []string{"workflow", "versions", "--json", "hello"}, `"version": 1`)
	wantCLI(t, []string{"workflow", "versions", "hello", "1"}, "echo HI")
	failCLI(t, "version must be an integer", "workflow", "versions", "hello", "x")
	failCLI(t, "", "workflow", "versions", "hello", "9")
	failCLI(t, "", "workflow", "versions", "nope")

	wantCLI(t, []string{"workflow", "disable", "hello"}, "hello is disabled")
	wantCLI(t, []string{"workflow", "enable", "hello"}, "hello is active")
	failCLI(t, "", "workflow", "enable", "nope")

	mustCLI(t, "workflow", "delete", "hello")
	failCLI(t, "", "workflow", "delete", "hello")
	wantCLI(t, []string{"schema"}, `"$schema"`)
}

func TestRunAndRunsCommands(t *testing.T) {
	startDaemon(t)
	dir := t.TempDir()
	mustCLI(t, "workflow", "apply", writeFile(t, dir, "hello.yaml", helloYAML))

	// A missing trigger field fails with a hint when prompting is off.
	failCLI(t, "--set who=<value>", "run", "hello", "--no-prompt")
	failCLI(t, "want key=value", "run", "hello", "--set", "who")
	failCLI(t, "--trigger-json", "run", "hello", "--trigger-json", "{")
	failCLI(t, "--trigger-file", "run", "hello", "--trigger-file", writeFile(t, dir, "bad.json", "nope"))
	failCLI(t, "no such file", "run", "hello", "--trigger-file", dir+"/absent.json")
	failCLI(t, "", "run", "nope", "--set", "who=x")

	// Docker is unreachable, so the engine never picks the run up: it
	// stays queued and every inspection command sees it as such.
	out := mustCLI(t, "run", "hello", "--set", "who=icaro")
	if !strings.Contains(out, "workflow=hello v1") || !strings.Contains(out, "status=queued") {
		t.Fatalf("run: %s", out)
	}
	id := strings.Fields(out)[1]
	out = mustCLI(t, "run", "hello", "--json", "--trigger-json", `{"who":"json"}`)
	var second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &second); err != nil || second.ID == "" {
		t.Fatalf("run --json: %v %s", err, out)
	}
	mustCLI(t, "run", "hello", "--trigger-file", writeFile(t, dir, "t.json", `{"who":"file"}`))

	wantCLI(t, []string{"runs", "list"}, id, "hello", "queued", "manual")
	wantCLI(t, []string{"runs", "list", "--json", "--workflow", "hello", "--limit", "1"}, `"workflow": "hello"`)
	wantCLI(t, []string{"runs", "get", id}, "run "+id, "STEP", "greet", "shout", "greet")
	wantCLI(t, []string{"runs", "get", "--json", id}, `"id": "`+id+`"`)
	failCLI(t, "", "runs", "get", "run_nope")

	// No step has an attempt yet, so logs print nothing.
	if out := mustCLI(t, "runs", "logs", id); out != "" {
		t.Errorf("logs of a queued run: %q", out)
	}
	failCLI(t, "", "runs", "logs", "run_nope")

	wantCLI(t, []string{"runs", "cancel", id}, "run "+id+" is cancelled")
	failCLI(t, "", "runs", "cancel", "run_nope")
	// Following a finished run returns at once with its final state.
	mustCLI(t, "runs", "logs", id, "--follow")
	mustCLI(t, "runs", "logs", id, "greet", "--follow")
}
