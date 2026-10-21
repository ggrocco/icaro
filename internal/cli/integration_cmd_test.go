package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/config"
	"icaro/internal/daemon"
	"icaro/internal/integration"
)

const acmeManifest = `version: 1
name: acme
description: Acme ticketing
hosts: [api.acme.dev]
auth:
  token: { kind: bearer, credential: token }
triggers:
  verify: { kind: hmac-sha256, header: X-Acme-Signature, encoding: hex }
  delivery_id: "${{ headers['x-acme-delivery'] }}"
  events:
    ticket.opened:
      match: "body.event == 'opened'"
      payload:
        ticket: "${{ body.ticket.id }}"
actions:
  close-ticket:
    inputs:
      ticket: { type: number, required: true }
    request:
      method: POST
      url: "https://api.acme.dev/tickets/${{ input.ticket }}/close"
      auth: token
    outputs:
      status: "${{ response.body.status }}"
`

const hookedYAML = `version: 1
name: hooked
triggers:
  - integration: webhook
    event: received
  - type: cron
    cron: "0 3 * * *"
steps:
  - id: noop
    image: mirror.gcr.io/library/alpine:3.20
    command: "true"
`

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := integration.ExecGit(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...); err != nil {
		t.Fatal(err)
	}
}

// acmeRepo creates a git repo holding one third-party manifest.
func acmeRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "acme-pack")
	if err := os.MkdirAll(filepath.Join(repo, "manifests"), 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "--quiet")
	writeFile(t, filepath.Join(repo, "manifests"), "acme.yaml", acmeManifest)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "acme")
	return repo
}

func layout(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := (&rootOpts{}).loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestIntegrationCommands(t *testing.T) {
	startDaemon(t)
	dir := t.TempDir()

	wantCLI(t, []string{"integration", "list"}, "NAME", "github", "webhook")
	wantCLI(t, []string{"integration", "list", "--json"}, `"name": "github"`)
	wantCLI(t, []string{"integration", "show", "github"}, "github (pack builtin", "hosts:", "event  ", "action github.")
	wantCLI(t, []string{"integration", "show", "github", "--manifest"}, "name: github")
	wantCLI(t, []string{"integration", "show", "--json", "github"}, `"name": "github"`)
	failCLI(t, "", "integration", "show", "nope")
	wantCLI(t, []string{"integration", "status"}, "PACK", "builtin")
	wantCLI(t, []string{"integration", "status", "--json"}, `"packs"`)
	wantCLI(t, []string{"integration", "schema"}, `"$schema"`)

	// Offline validation.
	good := writeFile(t, dir, "acme.yaml", acmeManifest)
	wantCLI(t, []string{"integration", "validate", good}, "valid: acme")
	failCLI(t, "issue(s)", "integration", "validate", writeFile(t, dir, "bad.yaml", "version: 1\nname: Bad Name\n"))
	failCLI(t, "no such file", "integration", "validate", dir+"/absent.yaml")

	// The local pack: edit commits a manifest, a second identical edit is
	// a no-op, and a missing file fails before reaching the daemon.
	wantCLI(t, []string{"integration", "edit", "acme", "-f", good}, "committed ", "to pack local")
	wantCLI(t, []string{"integration", "edit", "acme", "-f", good}, "unchanged")
	out, err := runCLI(t, acmeManifest, "integration", "edit", "acme", "-f", "-")
	if err != nil || !strings.Contains(out, "unchanged") {
		t.Fatalf("edit from stdin: %v %s", err, out)
	}
	failCLI(t, "no such file", "integration", "edit", "acme", "-f", dir+"/absent.yaml")
	failCLI(t, "", "integration", "edit", "acme", "-f", writeFile(t, dir, "broken.yaml", "version: 1\nname: acme\nhosts: 3\n"))
	// The local pack has no remote to push to.
	failCLI(t, "", "integration", "push", "local")
}

func TestIntegrationPackLifecycle(t *testing.T) {
	startDaemon(t)
	repo := acmeRepo(t)

	// Without --yes the summary is shown and a "no" keeps nothing.
	out, err := runCLI(t, "n\n", "integration", "add", "file://"+repo)
	if err == nil || !strings.Contains(out, "Activate this pack?") || !strings.Contains(out, "acme") {
		t.Fatalf("add without consent: %v\n%s", err, out)
	}
	out, err = runCLI(t, "yes\n", "integration", "add", "file://"+repo, "--name", "acme-pack")
	if err != nil || !strings.Contains(out, "installed pack acme-pack") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	wantCLI(t, []string{"integration", "list"}, "acme", "acme-pack")
	failCLI(t, "", "integration", "add", "file://"+filepath.Join(t.TempDir(), "missing"), "--yes")

	// An empty pack (no manifests) is refused.
	empty := filepath.Join(t.TempDir(), "empty-pack")
	if err := os.MkdirAll(empty, 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, empty, "init", "--quiet")
	writeFile(t, empty, "README", "nothing")
	gitIn(t, empty, "add", ".")
	gitIn(t, empty, "commit", "--quiet", "-m", "empty")
	failCLI(t, "no valid manifests", "integration", "add", "file://"+empty, "--yes")

	// Update pulls the new commit and prints the new summary.
	writeFile(t, filepath.Join(repo, "manifests"), "acme.yaml", strings.Replace(acmeManifest, "Acme ticketing", "Acme tickets v2", 1))
	gitIn(t, repo, "commit", "--quiet", "-am", "v2")
	wantCLI(t, []string{"integration", "update", "acme-pack"}, "acme")
	failCLI(t, "", "integration", "update", "nope")
	failCLI(t, "", "integration", "push", "nope")

	failCLI(t, "pass --yes", "integration", "remove", "acme-pack")
	mustCLI(t, "integration", "remove", "acme-pack", "--yes")
	failCLI(t, "", "integration", "remove", "acme-pack", "--yes")
}

func TestCredentialCommands(t *testing.T) {
	startDaemon(t)

	out, err := runCLI(t, "y\nghp_secret\n", "credential", "add", "github")
	wantCLI(t, []string{"credential", "list"}, "github.token", "api.github.com")
	if err != nil || !strings.Contains(out, "github.token will only be sent to: api.github.com") || !strings.Contains(out, "stored github.token") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	out, err = runCLI(t, "s3cret", "credential", "add", "github", "webhook_secret", "--yes")
	if err != nil || !strings.Contains(out, "verifies inbound webhooks") {
		t.Fatalf("add webhook secret: %v\n%s", err, out)
	}
	out, err = runCLI(t, "tok\n", "credential", "add", "github", "token", "--yes", "--host", "ghe.example.com")
	if err != nil || !strings.Contains(out, "only be sent to: ghe.example.com") {
		t.Fatalf("add with --host: %v\n%s", err, out)
	}
	if out, err := runCLI(t, "no\n", "credential", "add", "github"); err == nil || !strings.Contains(err.Error(), "not stored") {
		t.Fatalf("declined add: %v\n%s", err, out)
	}
	if out, err := runCLI(t, "\n", "credential", "add", "github", "--yes"); err == nil || !strings.Contains(err.Error(), "empty value") {
		t.Fatalf("empty value: %v\n%s", err, out)
	}
	failCLI(t, "", "credential", "add", "nope", "--yes")

	wantCLI(t, []string{"credential", "list"}, "NAME", "github.token", "github.webhook_secret", "ghe.example.com")
	failCLI(t, "pass --yes", "credential", "remove", "github.token")
	mustCLI(t, "credential", "remove", "github.token", "--yes")
	failCLI(t, "", "credential", "remove", "github.token", "--yes")
}

func TestWebhookDeliveriesAndSchedules(t *testing.T) {
	startDaemon(t)
	cfg := layout(t)
	dir := t.TempDir()
	mustCLI(t, "workflow", "apply", writeFile(t, dir, "hooked.yaml", hookedYAML))

	out := wantCLI(t, []string{"integration", "webhook-tokens"}, "hooked\tPOST ", "/hooks/webhook/")
	wantCLI(t, []string{"integration", "webhook-tokens", "--json"}, `"workflow": "hooked"`)
	rotated := wantCLI(t, []string{"integration", "webhook-tokens", "--rotate", "hooked"}, "hooked\tPOST ")
	if rotated == out {
		t.Errorf("rotate kept the token: %s", out)
	}
	failCLI(t, "", "integration", "webhook-tokens", "--rotate", "nope")
	path := rotated[strings.Index(rotated, "/hooks/"):]
	path = strings.TrimSpace(path)

	// Deliver one webhook over the socket, like a local tunnel would.
	hc := daemon.HTTPClient(cfg.Layout(), 0)
	req, _ := http.NewRequest(http.MethodPost, daemon.SocketBaseURL+path, strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "req-1")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("hook: %s", resp.Status)
	}

	out = wantCLI(t, []string{"deliveries", "list"}, "ID", "webhook", "processed")
	id := strings.Fields(strings.Split(out, "\n")[1])[0]
	wantCLI(t, []string{"deliveries", "list", "--json", "--integration", "webhook", "--status", "processed", "--limit", "5"}, `"id": "`+id+`"`)
	wantCLI(t, []string{"deliveries", "get", id}, `"integration": "webhook"`)
	failCLI(t, "", "deliveries", "get", "nope")

	out = wantCLI(t, []string{"deliveries", "redeliver", id}, "delivery ", "hooked (received)")
	var red struct {
		DeliveryID string `json:"delivery_id"`
	}
	if err := json.Unmarshal([]byte(mustCLI(t, "deliveries", "redeliver", "--json", id)), &red); err != nil || red.DeliveryID == "" {
		t.Fatalf("redeliver --json: %v", err)
	}
	wantCLI(t, []string{"deliveries", "list"}, "redelivery of "+id)
	failCLI(t, "", "deliveries", "redeliver", "nope")
	_ = out

	wantCLI(t, []string{"schedules"}, "WORKFLOW", "hooked", "0 3 * * *")
}
