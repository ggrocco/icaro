package session

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/container"
	"icaro/internal/crypto"
	"icaro/internal/inject"
	"icaro/internal/secrets"
	"icaro/internal/store"
)

const realAnthropicKey = "sk-ant-api03-REAL-SESSION-TEST-KEY-0123456789abcdefghijklmnop"

// relayEgress is fakeEgress for a profile whose proxy relays injection.
type relayEgress struct{ relay bool }

func (r relayEgress) Ensure(_ context.Context, profile string, _ []string) (container.Attachment, error) {
	att := container.Attachment{Network: container.NetworkName(profile), Env: []string{"HTTPS_PROXY=http://icaro-egress:3128", "NO_PROXY=localhost,127.0.0.1,icaro-egress"}}
	if r.relay {
		att.Inject = "http://icaro-egress:3129"
	}
	return att, nil
}

// injectEnv starts a manager whose claude profile injects the Anthropic
// key held in the secret store, as `icaro secret set anthropic-key` and
// `profiles.claude.inject.anthropic: anthropic-key` would set it up.
func newInjectEnv(t *testing.T, relay bool) *env {
	t.Helper()
	return newEnv(t, 4, func(o *Options) {
		p := o.Config.Profiles["claude"]
		p.Inject = map[string]string{"anthropic": "anthropic-key"}
		o.Config.Profiles["claude"] = p
		o.Egress = relayEgress{relay: relay}
		key, err := crypto.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := secrets.New(o.Store, key).Set(context.Background(), "anthropic-key", realAnthropicKey, []string{"api.anthropic.com"}); err != nil {
			t.Fatal(err)
		}
	})
}

func envValue(env []string, name string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v
		}
	}
	return ""
}

// TestInjectedSessionHoldsNoKey inspects the container create request
// built for an injecting session (env, labels, entrypoint, every mount
// and what it contains) and the profile home seed and session layer: the
// real key is in none of them; the container gets the relay URL and a
// session token instead.
func TestInjectedSessionHoldsNoKey(t *testing.T) {
	t.Parallel()
	e := newInjectEnv(t, true)
	ctx := context.Background()
	// A login left in the seed by an earlier `icaro login` stays as it was.
	seed, err := e.m.o.Homes.EnsureSeed("claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".claude.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "inj"})
	if err != nil || s.Status != StatusRunning {
		t.Fatalf("%+v %v", s, err)
	}
	spec := e.eng.spec(s.ContainerID)
	if got := envValue(spec.Env, "ANTHROPIC_BASE_URL"); got != "http://icaro-egress:3129/anthropic" {
		t.Fatalf("base URL %q", got)
	}
	token := envValue(spec.Env, "ANTHROPIC_API_KEY")
	if !strings.HasPrefix(token, inject.TokenPrefix+s.ID+".") || len(token) < len(inject.TokenPrefix)+len(s.ID)+40 {
		t.Fatalf("placeholder %q", token)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), realAnthropicKey) {
		t.Fatal("the real key is in the container create request")
	}
	// Every bind-mounted host path and the seed: no file holds the key.
	roots := []string{seed}
	for _, mt := range spec.HostConfig.Mounts {
		if mt.Source != "" && mt.Type == "bind" {
			roots = append(roots, mt.Source)
		}
	}
	scanned := 0
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			b, err := os.ReadFile(p) //nolint:gosec // test scan of its own temp directories
			if err == nil {
				scanned++
				if strings.Contains(string(b), realAnthropicKey) {
					t.Errorf("the real key is in %s, mounted into the container", p)
				}
			}
			return nil
		})
	}
	if scanned < 5 {
		t.Fatalf("scanned only %d files; the walk is not looking at the mounts", scanned)
	}
	// The token is stored only as a hash and authorises this session.
	h, err := e.st.InjectTokenHash(ctx, s.ID)
	if err != nil || h == token || h != crypto.HashToken(token) {
		t.Fatalf("stored %q %v", h, err)
	}
	if id, err := e.m.Authorize(ctx, "claude", token); err != nil || id != s.ID {
		t.Fatalf("%q %v", id, err)
	}
}

func TestInjectionTokenLifecycle(t *testing.T) {
	t.Parallel()
	e := newInjectEnv(t, true)
	ctx := context.Background()
	s, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "life"})
	if err != nil || s.Status != StatusRunning {
		t.Fatalf("%+v %v", s, err)
	}
	token := envValue(e.eng.spec(s.ContainerID).Env, "ANTHROPIC_API_KEY")
	refusedFor := func(name, profile, tok string) {
		t.Helper()
		if _, err := e.m.Authorize(ctx, profile, tok); !errors.Is(err, inject.ErrRefused) {
			t.Fatalf("%s: must be refused, got %v", name, err)
		}
	}
	refusedFor("another profile", "codex", token)
	refusedFor("tampered", "claude", token[:len(token)-2]+"xx")
	refusedFor("no prefix", "claude", strings.TrimPrefix(token, inject.TokenPrefix))
	refusedFor("no secret", "claude", inject.TokenPrefix+s.ID+".")
	refusedFor("no id", "claude", inject.TokenPrefix+".abc")
	refusedFor("unknown session", "claude", inject.TokenPrefix+"01UNKNOWN.abc")
	refusedFor("a real-looking key", "claude", realAnthropicKey)

	// Stopped: refused, and the hash is gone.
	if _, err := e.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	refusedFor("stopped session", "claude", token)
	if _, err := e.st.InjectTokenHash(ctx, s.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token hash kept after stop: %v", err)
	}
	// Restarted: a new token; the old one stays dead.
	s2, err := e.m.Start(ctx, s.ID)
	if err != nil || s2.Status != StatusRunning {
		t.Fatalf("%+v %v", s2, err)
	}
	token2 := envValue(e.eng.spec(s2.ContainerID).Env, "ANTHROPIC_API_KEY")
	if token2 == token || token2 == "" {
		t.Fatal("a restart must issue a new token")
	}
	refusedFor("previous start's token", "claude", token)
	if id, err := e.m.Authorize(ctx, "claude", token2); err != nil || id != s.ID {
		t.Fatalf("%q %v", id, err)
	}
	// Orphaned (its container died): refused.
	e.eng.kill(s2.ContainerID)
	e.m.probe(ctx)
	refusedFor("orphaned session", "claude", token2)
	if _, err := e.st.InjectTokenHash(ctx, s.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token hash kept after orphaning: %v", err)
	}
	// A running session whose hash was lost is refused, not let through.
	s3, err := e.m.Start(ctx, s.ID)
	if err != nil || s3.Status != StatusRunning {
		t.Fatalf("%+v %v", s3, err)
	}
	token3 := envValue(e.eng.spec(s3.ContainerID).Env, "ANTHROPIC_API_KEY")
	if err := e.st.DeleteInjectToken(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	refusedFor("missing hash", "claude", token3)
	// Removed: refused.
	if err := e.m.Remove(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	refusedFor("removed session", "claude", token3)
	// A run session is never an injection principal.
	ws, err := e.m.StartRun(ctx, "run-1", "claude")
	if err != nil {
		t.Fatal(err)
	}
	refusedFor("run session", "claude", inject.TokenPrefix+ws.SessionID+".abc")
	// The store failing is an error, not a refusal.
	_ = e.st.Close()
	if _, err := e.m.Authorize(ctx, "claude", token3); err == nil || errors.Is(err, inject.ErrRefused) {
		t.Fatalf("closed store: %v", err)
	}
}

func TestInjectionNeedsTheRelay(t *testing.T) {
	t.Parallel()
	e := newInjectEnv(t, false)
	s, err := e.m.Create(context.Background(), CreateRequest{RepoPath: e.repo, Name: "norelay"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusError || !strings.Contains(s.Error, "no injection relay") {
		t.Fatalf("a profile that injects must not start without the relay: %+v", s)
	}
	uid, gid := e.m.ContainerIDs()
	if uid != 1234 || gid != 1234 {
		t.Fatal(uid, gid)
	}
}

func TestProfilesWithoutInjectGetNoProviderEnv(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	s, err := e.m.Create(context.Background(), CreateRequest{RepoPath: e.repo, Name: "plain"})
	if err != nil || s.Status != StatusRunning {
		t.Fatalf("%+v %v", s, err)
	}
	spec := e.eng.spec(s.ContainerID)
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if envValue(spec.Env, k) != "" {
			t.Fatalf("%s set for a profile without inject", k)
		}
	}
	if _, err := e.st.InjectTokenHash(context.Background(), s.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("no token for a profile without inject")
	}
}
