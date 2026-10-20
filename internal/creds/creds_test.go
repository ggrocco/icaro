package creds

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/paths"
)

type fakeVolumes struct {
	created map[string]map[string]string
	removed []string
}

func (f *fakeVolumes) CreateVolumeOpts(_ context.Context, name, _ string, opts, _ map[string]string) error {
	if f.created == nil {
		f.created = map[string]map[string]string{}
	}
	f.created[name] = opts
	return nil
}

func (f *fakeVolumes) RemoveVolume(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

func TestCopyModeIsolatesSessions(t *testing.T) {
	h := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeCopy}
	seed, _ := h.EnsureSeed("claude")
	_ = os.MkdirAll(filepath.Join(seed, ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(seed, ".claude", ".credentials.json"), []byte(`{"token":"t"}`), 0o600)
	ctx := context.Background()
	a, err := h.Prepare(ctx, "claude", "A")
	if err != nil || a.Dir == "" {
		t.Fatal(a, err)
	}
	b, _ := h.Prepare(ctx, "claude", "B")
	// Both see the login...
	for _, m := range []string{a.Dir, b.Dir} {
		if got, _ := os.ReadFile(filepath.Join(m, ".claude", ".credentials.json")); string(got) != `{"token":"t"}` {
			t.Fatalf("login not shared: %q", got)
		}
	}
	// ...and their writes stay apart and never reach the seed.
	_ = os.WriteFile(filepath.Join(a.Dir, ".claude", "history"), []byte("A"), 0o600)
	_ = os.WriteFile(filepath.Join(b.Dir, ".claude", "history"), []byte("B"), 0o600)
	if got, _ := os.ReadFile(filepath.Join(a.Dir, ".claude", "history")); string(got) != "A" {
		t.Fatal("A corrupted")
	}
	if _, err := os.Stat(filepath.Join(seed, ".claude", "history")); !os.IsNotExist(err) {
		t.Fatal("seed written by a session")
	}
	// Prepare is idempotent: state survives a stop/start.
	again, _ := h.Prepare(ctx, "claude", "A")
	if got, _ := os.ReadFile(filepath.Join(again.Dir, ".claude", "history")); string(got) != "A" {
		t.Fatal("layer not reused")
	}
	if err := h.Release(ctx, "claude", "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Dir); !os.IsNotExist(err) {
		t.Fatal("release")
	}
}

func TestOverlayModeVolume(t *testing.T) {
	vols := &fakeVolumes{}
	h := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeOverlay, Volumes: vols}
	m, err := h.Prepare(context.Background(), "codex", "S1")
	if err != nil || m.Volume != VolumeName("S1") {
		t.Fatal(m, err)
	}
	o := vols.created[m.Volume]
	if o["type"] != "overlay" || !strings.Contains(o["o"], "lowerdir="+h.SeedDir("codex")) || !strings.Contains(o["o"], "upperdir=") {
		t.Fatalf("%v", o)
	}
	// Files written for the session land in the upper layer.
	if err := h.Write("codex", "S1", ".codex/x", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.Layout.Home("codex"), "sessions", "S1", "upper", ".codex", "x")); err != nil {
		t.Fatal(err)
	}
	if err := h.Write("codex", "S1", "../escape", nil); err == nil {
		t.Fatal("path escape")
	}
	_ = h.Release(context.Background(), "codex", "S1")
	if len(vols.removed) != 1 {
		t.Fatal("volume not removed")
	}
	auto := &Homes{Layout: h.Layout, Mode: ModeAuto, Volumes: vols}
	auto.DisableOverlay()
	if auto.OverlayEnabled() {
		t.Fatal("DisableOverlay")
	}
}

func TestAgentConfiguration(t *testing.T) {
	h := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeCopy}
	seed, _ := h.EnsureSeed("claude")
	_ = os.WriteFile(filepath.Join(seed, ".claude.json"), []byte(`{"userID":"u","mcpServers":{"other":{"command":"x"}}}`), 0o600)
	_ = os.MkdirAll(filepath.Join(seed, ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(seed, ".claude", "CLAUDE.md"), []byte("# mine\n"), 0o600)
	if _, err := h.Prepare(context.Background(), "claude", "S"); err != nil {
		t.Fatal(err)
	}
	a, _ := LookupAgent("claude")
	files := h.SessionFiles("claude", "S")
	for i := 0; i < 2; i++ { // idempotent
		if err := a.Configure(files, true, "Accept handoffs."); err != nil {
			t.Fatal(err)
		}
	}
	raw, _, _ := files.Read(".claude.json")
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	if cfg["userID"] != "u" || servers["other"] == nil || servers["icaro"].(map[string]any)["command"] != "icaro" {
		t.Fatalf("%s", raw)
	}
	md, _, _ := files.Read(".claude/CLAUDE.md")
	if !strings.HasPrefix(string(md), "# mine") || strings.Count(string(md), preambleStart) != 1 || !strings.Contains(string(md), "Accept handoffs.") {
		t.Fatalf("%q", md)
	}
	seedCfg, _ := os.ReadFile(filepath.Join(seed, ".claude.json"))
	if strings.Contains(string(seedCfg), "icaro") {
		t.Fatal("seed modified")
	}

	c, _ := LookupAgent("codex")
	h2 := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeCopy}
	_, _ = h2.Prepare(context.Background(), "codex", "S")
	cf := h2.SessionFiles("codex", "S")
	for i := 0; i < 2; i++ {
		if err := c.Configure(cf, true, "P"); err != nil {
			t.Fatal(err)
		}
	}
	toml, _, _ := cf.Read(".codex/config.toml")
	if strings.Count(string(toml), "[mcp_servers.icaro]") != 1 || !strings.Contains(string(toml), `"--socket"`) {
		t.Fatalf("%s", toml)
	}
	if _, err := LookupAgent("gemini"); err == nil {
		t.Fatal("unknown agent")
	}
	sh, _ := LookupAgent("")
	if err := sh.Configure(cf, true, "x"); err != nil {
		t.Fatal(err)
	}
	if got := withPreamble([]byte("a\n"+preambleStart+"\nold\n"+preambleEnd+"\n"), ""); string(got) != "a\n" {
		t.Fatalf("%q", got)
	}
}
