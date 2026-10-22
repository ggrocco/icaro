package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExecGitErrorWithoutStderr(t *testing.T) {
	// `git config --get` of an unset key fails silently.
	_, err := ExecGit(context.Background(), t.TempDir(), "config", "--file", filepath.Join(t.TempDir(), "empty"), "--get", "no.such")
	if err == nil || !strings.Contains(err.Error(), "exit status") {
		t.Fatal(err)
	}
}

func TestSummaryFormat(t *testing.T) {
	m, issues, err := Parse([]byte(gammaManifest))
	if err != nil || len(issues) > 0 {
		t.Fatal(err, issues)
	}
	noActions, _, _ := Parse([]byte("version: 1\nname: bare\n"))
	if s := Summarize(noActions); s.Actions == nil || len(s.Actions) != 0 {
		t.Fatalf("no actions is an empty list: %+v", s)
	}
	if s := Summarize(m); !slices.Contains(s.Hosts, "(user-configured base_url)") {
		t.Fatalf("a base_url without a default is user-configured: %+v", s.Hosts)
	}

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "github.yaml"), []byte(strings.Replace(acmeManifest, "name: acme", "name: github", 1)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("version: 2\n"), 0o600)
	sum := SummarizeDir("mine", dir)
	text := sum.Format()
	for _, want := range []string{"pack mine", "integration github  [shadows the builtin]", "verify:  hmac-sha256", "INVALID broken.yaml"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	empty := Summary{Pack: "none"}.Format()
	if !strings.Contains(empty, "(no valid manifests)") {
		t.Fatal(empty)
	}
	if orNone(nil) != "(none)" {
		t.Fatal("orNone")
	}
	if got := PackNameFromURL("host:pack"); got != "pack" {
		t.Fatal(got)
	}
}

// failGit runs real git but fails the first command whose arguments start
// with fail.
func failGit(fail ...string) GitRunner {
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		// Without a git identity, commits run as `git -c user.name=... commit`.
		sub := args
		for len(sub) >= 2 && sub[0] == "-c" {
			sub = sub[2:]
		}
		if len(sub) >= len(fail) && slices.Equal(sub[:len(fail)], fail) {
			return "", errors.New("git " + strings.Join(fail, " ") + ": injected failure")
		}
		return ExecGit(ctx, dir, args...)
	}
}

func TestPacksManagement(t *testing.T) {
	ctx := context.Background()
	repo := thirdPartyRepo(t)
	dir := t.TempDir()
	p := NewPacks(dir, nil)
	if p.List() != nil {
		t.Fatal("no packs yet")
	}
	st, err := p.Stage(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(st); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("discard leaves nothing behind: %v", entries)
	}
	st, err = p.Stage(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Activate(st); err != nil {
		t.Fatal(err)
	}
	if got := p.List(); !slices.Equal(got, []string{"acme-pack"}) {
		t.Fatal(got)
	}
	if _, err := p.Update(ctx, "acme-pack"); err != nil {
		t.Fatal(err)
	}
	// The clone's remote is a non-bare repo with main checked out: the push
	// is refused, and the error is returned.
	git(t, filepath.Join(dir, "acme-pack"), "commit", "--quiet", "--allow-empty", "-m", "local")
	if err := p.Push(ctx, "acme-pack"); err == nil {
		t.Fatal("push to a checked-out branch must fail")
	}
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, "", "clone", "--quiet", "--bare", repo, bare)
	git(t, filepath.Join(dir, "acme-pack"), "remote", "set-url", "origin", bare)
	if err := p.Push(ctx, "acme-pack"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{PackBuiltin, "Bad Name", "missing"} {
		if _, err := p.Update(ctx, name); err == nil {
			t.Errorf("update %q must fail", name)
		}
		if err := p.Push(ctx, name); err == nil {
			t.Errorf("push %q must fail", name)
		}
	}
	p.Git = failGit("pull")
	if _, err := p.Update(ctx, "acme-pack"); err == nil {
		t.Fatal("pull errors are returned")
	}

	// The integrations directory cannot be created.
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := NewPacks(filepath.Join(file, "sub"), nil).Stage(ctx, repo, "x"); err == nil {
		t.Fatal("stage into an impossible directory")
	}
}

func TestEnsureLocalFailures(t *testing.T) {
	ctx := context.Background()
	for _, fail := range [][]string{{"init"}, {"add"}, {"commit"}} {
		p := NewPacks(t.TempDir(), failGit(fail...))
		if _, err := p.EnsureLocal(ctx); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Errorf("%v: %v", fail, err)
		}
	}
	// The local pack path is taken by a file.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, PackLocal), nil, 0o600)
	if _, err := NewPacks(dir, nil).EnsureLocal(ctx); err == nil {
		t.Fatal("mkdir error")
	}
	// The README cannot be written: it is a directory.
	dir = t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, PackLocal, "README.md"), 0o700)
	if _, err := NewPacks(dir, nil).EnsureLocal(ctx); err == nil {
		t.Fatal("write error")
	}
}

func TestEditManifestFailures(t *testing.T) {
	ctx := context.Background()
	p := NewPacks(t.TempDir(), nil)
	var ve *ValidationError
	if _, err := p.EditManifest(ctx, "acme", []byte("a: ["), ""); !errors.As(err, &ve) || !strings.Contains(err.Error(), "manifest is invalid") {
		t.Fatal(err)
	}
	if _, err := p.EditManifest(ctx, "other", []byte(acmeManifest), ""); !errors.As(err, &ve) || !strings.Contains(err.Error(), `does not match "other"`) {
		t.Fatal(err)
	}
	if _, err := NewPacks(t.TempDir(), failGit("init")).EditManifest(ctx, "acme", []byte(acmeManifest), ""); err == nil {
		t.Fatal("ensure local errors are returned")
	}

	// Without a configured identity the commit supplies one.
	var commits [][]string
	noIdentity := func(ctx context.Context, dir string, args ...string) (string, error) {
		if slices.Equal(args, []string{"config", "user.email"}) {
			return "", nil
		}
		if slices.Contains(args, "commit") {
			commits = append(commits, args)
		}
		return ExecGit(ctx, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	}
	p = NewPacks(t.TempDir(), noIdentity)
	if _, err := p.EditManifest(ctx, "acme", []byte(acmeManifest), ""); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || !slices.Contains(commits[1], "user.email=icaro@localhost") {
		t.Fatalf("%v", commits)
	}
	if !strings.Contains(commits[1][len(commits[1])-1], "Edited-By: icaro") {
		t.Fatalf("the default actor is icaro: %v", commits[1])
	}
	// A changed manifest is an update.
	if _, err := p.EditManifest(ctx, "acme", []byte(acmeManifest+"# v2\n"), "alice"); err != nil {
		t.Fatal(err)
	}
	if msg := commits[len(commits)-1]; !strings.Contains(msg[len(msg)-1], "icaro: update acme manifest") {
		t.Fatalf("%v", msg)
	}

	for _, fail := range [][]string{{"add", "--"}, {"commit"}} {
		p := NewPacks(t.TempDir(), nil)
		if _, err := p.EnsureLocal(ctx); err != nil {
			t.Fatal(err)
		}
		p.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
			if slices.Equal(args, []string{"config", "user.email"}) {
				return "", nil
			}
			return failGit(fail...)(ctx, dir, args...)
		}
		if _, err := p.EditManifest(ctx, "acme", []byte(acmeManifest), ""); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Errorf("%v: %v", fail, err)
		}
	}

	// The manifest path is taken by a directory.
	p = NewPacks(t.TempDir(), nil)
	dir, err := p.EnsureLocal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "acme.yaml"), 0o700)
	if _, err := p.EditManifest(ctx, "acme", []byte(acmeManifest), ""); err == nil {
		t.Fatal("write error")
	}
}
