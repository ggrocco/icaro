package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillInstallClaudeAndCodex(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"dark","mcpServers":{"other":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"claude", "codex", "codex"} {
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"skill", "install", "--target", target, "--home-dir", home})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "icaro-workflows", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "validate_workflow") {
		t.Fatalf("skill: %v", err)
	}
	var cfg map[string]any
	raw, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if cfg["theme"] != "dark" || servers["other"] == nil || servers["icaro"] == nil {
		t.Fatalf("merge lost keys: %s", raw)
	}
	toml, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if strings.Count(string(toml), "[mcp_servers.icaro]") != 1 {
		t.Fatalf("codex config: %s", toml)
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"skill", "install", "nope", "--home-dir", home})
	if err := cmd.Execute(); err == nil {
		t.Fatal("unknown skill accepted")
	}
}
