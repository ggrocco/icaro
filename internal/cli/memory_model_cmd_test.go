package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"icaro/internal/assist/distill"
)

func TestMemoryModelCommands(t *testing.T) {
	home := daemonEnv(t)
	payload := []byte("tiny gguf")
	sum := sha256.Sum256(payload)
	tiny := distill.Model{Name: "tiny", Repo: "org/tiny", File: "tiny.gguf", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Tier: distill.TierMinimal, License: "MIT"}
	bad := distill.Model{Name: "bad", Repo: "org/bad", File: "bad.gguf", SHA256: tiny.SHA256, Size: 3, Tier: distill.TierMinimal}
	saved := distill.Catalog
	distill.Catalog = append([]distill.Model{tiny, bad}, saved...)
	t.Cleanup(func() { distill.Catalog = saved })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/tiny/resolve/main/tiny.gguf":
			_, _ = w.Write(payload)
		case "/org/bad/resolve/main/bad.gguf":
			_, _ = w.Write([]byte("bad"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	modelsBaseURL = srv.URL
	t.Cleanup(func() { modelsBaseURL = "" })

	wantCLI(t, []string{"memory", "model", "list"}, "This machine's tier:", "NAME", "tiny", "9 B", "qwen3.5-2b", "1.2 GiB")
	wantCLI(t, []string{"memory", "model", "pull", "tiny"}, "pulled tiny")
	if _, err := os.Stat(filepath.Join(home, "models", "tiny.gguf")); err != nil {
		t.Fatal(err)
	}
	wantCLI(t, []string{"memory", "model", "list", "--json"}, `"name": "tiny"`, `"pulled": true`)
	failCLI(t, "does not match", "memory", "model", "pull", "bad")
	failCLI(t, "404", "memory", "model", "pull", "granite-4.0-1b")
	failCLI(t, "unknown model", "memory", "model", "pull", "nope")
	failCLI(t, "unknown model", "memory", "model", "rm", "nope", "--yes")
	wantCLI(t, []string{"memory", "model", "rm", "tiny"}, "Delete model tiny (9 B)? [y/N]") // no answer: kept
	if _, err := os.Stat(filepath.Join(home, "models", "tiny.gguf")); err != nil {
		t.Fatal(err)
	}
	mustCLI(t, "memory", "model", "rm", "tiny", "--yes")
	if _, err := os.Stat(filepath.Join(home, "models", "tiny.gguf")); !os.IsNotExist(err) {
		t.Fatal("model kept", err)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
