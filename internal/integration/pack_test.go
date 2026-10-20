package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := ExecGit(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// thirdPartyRepo creates a git repo holding the acme manifest.
func thirdPartyRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "acme-pack")
	if err := os.MkdirAll(filepath.Join(repo, "manifests"), 0o750); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repo, "manifests", "acme.yaml"), []byte(acmeManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "--quiet", "-m", "acme")
	return repo
}

func TestAddThirdPartyPackWithoutRebuild(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reg := NewRegistry(dir, nil)
	if _, ok := reg.Snapshot().Action("acme.close-ticket"); ok {
		t.Fatal("acme should not exist yet")
	}
	packs := NewPacks(dir, nil)
	repo := thirdPartyRepo(t)
	staged, err := packs.Stage(ctx, "file://"+repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if staged.Name != "acme-pack" {
		t.Fatalf("name %q", staged.Name)
	}
	text := staged.Summary.Format()
	for _, want := range []string{"integration acme", "hosts:   api.acme.dev", "token: bearer (credential acme.token)", "verify:  hmac-sha256", "events:  ticket.opened", "actions: close-ticket"} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary missing %q:\n%s", want, text)
		}
	}
	// Staged but not activated: invisible.
	_ = reg.Reload(ctx)
	if _, ok := reg.Snapshot().Get("acme"); ok {
		t.Fatal("staged pack must not load")
	}
	if err := packs.Activate(staged); err != nil {
		t.Fatal(err)
	}
	_ = reg.Reload(ctx)
	snap := reg.Snapshot()
	info, ok := snap.Action("acme.close-ticket")
	if !ok || !info.Inputs["ticket"].Required || info.Outputs[0] != "status" {
		t.Fatalf("action: %+v %v", info, ok)
	}
	ev, ok := snap.Event("acme", "ticket.opened")
	if !ok || ev.Fields[0] != "ticket" {
		t.Fatalf("event %+v", ev)
	}
	var st PackStatus
	for _, p := range snap.Packs {
		if p.Name == "acme-pack" {
			st = p
		}
	}
	if !strings.HasPrefix(st.Remote, "file://") || st.Head == "" || st.Integrations[0] != "acme" {
		t.Fatalf("status %+v", st)
	}
	if _, err := packs.Stage(ctx, "file://"+repo, ""); err == nil {
		t.Fatal("re-adding an installed pack must fail")
	}
	// Update pulls new commits.
	if err := os.WriteFile(filepath.Join(repo, "manifests", "acme.yaml"), []byte(strings.Replace(acmeManifest, "Acme ticketing", "Acme v2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "--quiet", "-am", "v2")
	sum, err := packs.Update(ctx, "acme-pack")
	if err != nil || sum.Integrations[0].Description != "Acme v2" {
		t.Fatalf("update: %+v %v", sum, err)
	}
	if err := packs.Remove("acme-pack"); err != nil {
		t.Fatal(err)
	}
	if err := packs.Remove(PackBuiltin); err == nil {
		t.Fatal("builtin is read-only")
	}
}

func TestStageRejectsBadNames(t *testing.T) {
	p := NewPacks(t.TempDir(), nil)
	for _, n := range []string{"builtin", "local", "Bad Name"} {
		if _, err := p.Stage(context.Background(), "file:///nope", n); err == nil {
			t.Fatalf("%q accepted", n)
		}
	}
	if _, err := p.Stage(context.Background(), "file:///definitely/not/here.git", "x"); err == nil {
		t.Fatal("clone failure")
	}
	if PackNameFromURL("git@github.com:org/My-Pack.git") != "my-pack" {
		t.Fatal(PackNameFromURL("git@github.com:org/My-Pack.git"))
	}
}

func TestEditManifestCommitsAndKeepsLastGood(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packs := NewPacks(dir, nil)
	reg := NewRegistry(dir, nil)

	// An invalid edit is rejected before any write.
	_, err := packs.EditManifest(ctx, "acme", []byte(strings.Replace(acmeManifest, "close-ticket:", "close-ticket:\n    bogus: 1", 1)), "ui")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want validation error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, PackLocal)); err == nil {
		t.Fatal("invalid edit must not create or write the local pack")
	}

	hash, err := packs.EditManifest(ctx, "acme", []byte(acmeManifest), "ui:alice")
	if err != nil || len(hash) < 7 {
		t.Fatalf("edit: %q %v", hash, err)
	}
	logMsg := git(t, filepath.Join(dir, PackLocal), "log", "-1", "--format=%B")
	if !strings.Contains(logMsg, "icaro: add acme manifest") || !strings.Contains(logMsg, "Edited-By: ui:alice") {
		t.Fatal(logMsg)
	}
	if h, err := packs.EditManifest(ctx, "acme", []byte(acmeManifest), "ui"); err != nil || h != "" {
		t.Fatalf("no-op edit: %q %v", h, err)
	}
	_ = reg.Reload(ctx)
	if _, ok := reg.Snapshot().Action("acme.close-ticket"); !ok {
		t.Fatal("edit not served")
	}

	// A broken manifest committed by hand keeps the last good version.
	broken := strings.Replace(acmeManifest, "close-ticket:", "close-ticket:\n    bogus: 1", 1)
	if err := os.WriteFile(filepath.Join(dir, PackLocal, "acme.yaml"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Join(dir, PackLocal), "commit", "--quiet", "-am", "break it")
	_ = reg.Reload(ctx)
	snap := reg.Snapshot()
	if _, ok := snap.Action("acme.close-ticket"); !ok {
		t.Fatal("last good version must keep serving")
	}
	errs := snap.Errors()
	if len(errs) != 1 || !errs[0].ServingLastGood || !strings.Contains(errs[0].Error, "bogus") {
		t.Fatalf("errors: %+v", errs)
	}
}

func TestShadowAndCollision(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := func(pack, file, src string) {
		p := filepath.Join(dir, pack)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, file), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	slack := strings.Replace(string(builtinFiles()["slack.yaml"]), "description: Slack channel", "description: My Slack channel", 1)
	write("mine", "slack.yaml", slack)
	write("a", "acme.yaml", acmeManifest)
	write("b", "acme.yaml", acmeManifest)
	write("c", "wrongname.yaml", acmeManifest)
	reg := NewRegistry(dir, nil)
	snap := reg.Snapshot()
	l, ok := snap.Get("slack")
	if !ok || l.Pack != "mine" || !l.Shadows || len(snap.Warnings) != 1 {
		t.Fatalf("shadow: %+v %v", l, snap.Warnings)
	}
	if _, ok := snap.Get("acme"); ok {
		t.Fatal("colliding user packs must not load")
	}
	text := ""
	for _, e := range snap.Errors() {
		text += e.Error + "\n"
	}
	if !strings.Contains(text, "more than one user pack (a, b)") || !strings.Contains(text, `must match its file name "wrongname"`) {
		t.Fatal(text)
	}
	// The watcher notices new files.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = reg.Run(runCtx, 20*time.Millisecond) }()
	if err := os.RemoveAll(filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Snapshot().Get("acme"); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := reg.Snapshot().Get("acme"); !ok {
		t.Fatal("watcher did not reload")
	}
	if err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	schema, ok := reg.Snapshot().ActionInputSchema("acme.close-ticket")
	if !ok || schema["required"].([]string)[0] != "ticket" {
		t.Fatalf("%v", schema)
	}
	c := Catalog{R: reg}
	if len(c.ActionRefs()) == 0 || len(c.Integrations()) < 9 || len(c.EventNames("github")) != 5 {
		t.Fatalf("catalog %v %v", c.Integrations(), c.EventNames("github"))
	}
}
