//go:build integration

package container

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/inject"
)

const itKey = "sk-ant-api03-INTEGRATION-0123456789abcdefghijklmnopqrstuvwxyz"

type itAuth struct{}

func (itAuth) Authorize(_ context.Context, profile, token string) (string, error) {
	if token == inject.TokenPrefix+"SESSION.tok" {
		return "SESSION", nil
	}
	return "", inject.ErrRefused
}

type itKeys struct{}

func (itKeys) ResolveFor(context.Context, string, string) (string, error) { return itKey, nil }

// TestCredentialInjectionThroughTheProxy runs the real path: a container
// on the profile network calls http://icaro-egress:3129/anthropic/...
// with only its placeholder; the proxy container relays it over the
// bind-mounted socket to the injector running here, on the host, which
// adds the key and calls a TLS provider. The container sees the answer,
// never the key, and the proxy container's spec holds no key either.
func TestCredentialInjectionThroughTheProxy(t *testing.T) {
	eng := engine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != itKey || r.URL.Path != "/v1/models" {
			http.Error(w, "bad injection", http.StatusTeapot)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"model-1"}],"echo":"`+itKey+`"}`)
	}))
	defer provider.Close()

	runDir, err := os.MkdirTemp("", "icit") // short: Unix socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 1000, 1000
	}
	profile := "it" + strings.ToLower(time.Now().Format("150405"))
	srv := &inject.Server{
		Profiles:   map[string]map[string]string{profile: {"anthropic": "anthropic-key"}},
		SocketPath: func(p string) string { return filepath.Join(InjectSocketDir(runDir, p), InjectSocketName) },
		Auth:       itAuth{}, Keys: itKeys{},
		Transport: provider.Client().Transport, Upstreams: map[string]string{"anthropic": provider.URL},
		UID: uid, GID: gid,
	}
	go func() { _ = srv.Run(ctx) }()

	eg := &DockerEgress{Engine: eng, HelperBinary: staticIcaro(t), InjectUser: fmt.Sprintf("%d:%d", uid, gid),
		InjectDir: func(p string) string { return InjectSocketDir(runDir, p) }}
	att, err := eg.Ensure(ctx, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids, _ := eng.ListByLabel(context.Background(), map[string]string{LabelEgress: profile})
		for _, id := range ids {
			_ = eng.RemoveContainer(context.Background(), id)
		}
		_ = eng.RemoveNetwork(context.Background(), NetworkName(profile))
	})
	if att.Inject == "" {
		t.Fatal("no injection endpoint")
	}
	time.Sleep(time.Second)

	env := inject.Env(att.Inject, map[string]string{"anthropic": "anthropic-key"}, inject.TokenPrefix+"SESSION.tok")
	client := func(script string, extra ...string) (string, error) {
		args := append([]string{"run", "--rm", "--network", att.Network}, runtimeArgs(att)...)
		for _, e := range append(append(append([]string{}, att.Env...), env...), extra...) {
			args = append(args, "-e", e)
		}
		args = append(args, testImage, "sh", "-c", script)
		return run(t, args...)
	}
	out, err := client(`env | grep -q '` + itKey + `' && echo KEY-IN-ENV; wget -q -T 10 -O - --header "x-api-key: $ANTHROPIC_API_KEY" "$ANTHROPIC_BASE_URL/v1/models"`)
	if err != nil || !strings.Contains(out, "model-1") {
		t.Fatalf("injected call: %v %q", err, out)
	}
	if strings.Contains(out, itKey) || strings.Contains(out, "KEY-IN-ENV") {
		t.Fatalf("the key reached the container: %q", out)
	}
	// A wrong token and a path off the allowlist are refused.
	if out, err := client(`wget -q -T 10 -O - --header "x-api-key: icaro-injected.OTHER.x" "$ANTHROPIC_BASE_URL/v1/models"`); err == nil {
		t.Fatalf("a foreign token must be refused: %q", out)
	}
	if out, err := client(`wget -q -T 10 -O - --header "x-api-key: $ANTHROPIC_API_KEY" "$ANTHROPIC_BASE_URL/v1/organizations"`); err == nil {
		t.Fatalf("a path off the allowlist must be refused: %q", out)
	}
	// The proxy container holds no key: not in its env, entrypoint or mounts.
	ids, _ := eng.ListByLabel(ctx, map[string]string{LabelEgress: profile})
	for _, id := range ids {
		insp, err := run(t, "inspect", id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(insp, itKey) {
			t.Fatal("the key is in the proxy container's spec")
		}
	}
}
