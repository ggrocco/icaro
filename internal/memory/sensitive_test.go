package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/store"
)

const (
	stripeKey = "sk_" + "live_51HxQabcdefghijklmnopqrstuv" // split so secret scanners do not flag the fixture
	ghToken   = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
)

// sensitiveTree is a repository with secrets in every place ingest used
// to read them from.
func sensitiveTree() map[string]string {
	return map[string]string{
		".gitignore":          "*.log\nbuild-out/\n",
		".env":                "STRIPE_SECRET_KEY=" + stripeKey + "\n",
		".npmrc":              "//registry.npmjs.org/:_authToken=npm_secretvalue123\n",
		"debug.log":           "stripe secret key " + stripeKey + "\n",
		"build-out/dump.txt":  "stripe secret key " + stripeKey + "\n",
		"deploy/server.pem":   "-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----\n",
		"deploy/id_ed25519":   "openssh private key",
		"credentials.json":    `{"stripe_secret": "` + stripeKey + `"}`,
		"config/app.yaml":     "stripe:\n  secret_key: " + stripeKey + "\n  password: hunter2hunter2\n  region: eu\n",
		"config/settings.ini": "[stripe]\napi_key = plainvalue123\n",
		"web/client.js":       "export const GITHUB = \"" + ghToken + "\"\nexport function stripeSecretKey() {}\n",
		"scripts/env.sh":      "export STRIPE_SECRET_KEY=" + stripeKey + "\n",
		"internal/secrets.go": "package internal\n\n// Vault holds the stripe secret key.\ntype Vault struct{}\n",
	}
}

func allBodies(t *testing.T, e *Engine, scope Scope) string {
	t.Helper()
	var sb strings.Builder
	for _, q := range []Query{{Text: "stripe secret key", Limit: 200}, {Kinds: []string{KindFile, KindSymbol}, Limit: 200}} {
		res, err := e.Query(context.Background(), scope, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res {
			sb.WriteString(m.Source + ": " + m.Title + "\n" + m.Body + "\n")
		}
	}
	return sb.String()
}

// Regression: `memory_query "stripe secret key"` returned the .env line
// with a live key. Ingest skips dotfiles, ignored files and key material,
// and redacts what it stores of the rest.
func TestIngestKeepsSecretsOut(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		root := t.TempDir()
		writeFiles(t, root, sensitiveTree())
		gitInit(t, root)
		pid, err := ProjectIDForPath(root)
		if err != nil {
			t.Fatal(err)
		}
		scope := Scope{ProjectID: pid, Origin: OriginIngest}
		rep, err := e.Ingest(ctx, scope, root, IngestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// .env, .npmrc, debug.log, server.pem, id_ed25519, credentials.json
		// (build-out/ is skipped as a directory).
		if rep.Excluded != 6 {
			t.Fatalf("report: %+v", rep)
		}
		text := allBodies(t, e, scope)
		for _, leak := range []string{stripeKey, ghToken, "hunter2hunter2", "plainvalue123", "npm_secretvalue123", "PRIVATE KEY", "openssh"} {
			if strings.Contains(text, leak) {
				t.Errorf("%q is served from memory:\n%s", leak, text)
			}
		}
		for _, want := range []string{"internal/secrets.go", "config/app.yaml", "secret_key: ***", "region: eu", "STRIPE_SECRET_KEY=***", "stripeSecretKey", ".gitignore"} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q:\n%s", want, text)
			}
		}

		// A row stored by an older ingest (unredacted, plain content hash)
		// is re-indexed although the file did not change.
		content := []byte(sensitiveTree()["config/app.yaml"])
		sum := sha256.Sum256(content)
		old := &store.MemoryRow{
			ID: stableID(pid, KindFile, "config/app.yaml"), ProjectID: pid, Origin: OriginIngest, Kind: KindFile,
			Title: "config/app.yaml", Body: string(content), Source: "config/app.yaml", ContentHash: hex.EncodeToString(sum[:]),
			Metadata: "{}", CreatedAt: e.now(), UpdatedAt: e.now(),
		}
		if err := e.db.PutMemories(ctx, []store.MemoryBundle{bundle(old)}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(allBodies(t, e, scope), stripeKey) {
			t.Fatal("setup: the old row must be visible")
		}
		if _, err := e.Ingest(ctx, scope, root, IngestOptions{}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(allBodies(t, e, scope), stripeKey) {
			t.Fatal("a row from an older ingest must be re-indexed with redaction")
		}
	})
}

// Outside git, ignore rules are unknown: name rules and redaction still
// apply.
func TestIngestOutsideGitStillRedacts(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		root := t.TempDir()
		writeFiles(t, root, sensitiveTree())
		scope := Scope{ProjectID: "p-nogit", Origin: OriginIngest}
		rep, err := e.Ingest(context.Background(), scope, root, IngestOptions{Commits: -1})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Excluded != 5 { // the ignored files are indexed, redacted
			t.Fatalf("report: %+v", rep)
		}
		text := allBodies(t, e, scope)
		if strings.Contains(text, stripeKey) || !strings.Contains(text, filepath.ToSlash("debug.log")) {
			t.Fatalf("%s", text)
		}
	})
}

func TestSensitiveFileNames(t *testing.T) {
	for name, want := range map[string]bool{
		".env": true, ".env.local": true, ".npmrc": true, ".gitignore": false, ".editorconfig": false,
		"tls.key": true, "cert.PEM": true, "store.p12": true, "id_rsa": true, "id_rsa.pub": true, "id_test.go": false,
		"credentials": true, "credentials.json": true, "credentials.go": false, "secrets.yaml": true, "secrets.go": false,
		"main.go": false, "README.md": false,
	} {
		if got := sensitiveFile(name); got != want {
			t.Errorf("sensitiveFile(%q) = %v", name, got)
		}
	}
	if got := redactText("token := next()\nAPI_TOKEN=abc123", "go"); got != "token := next()\nAPI_TOKEN=***" {
		t.Fatalf("code keeps code, env assignments are redacted: %q", got)
	}
}
