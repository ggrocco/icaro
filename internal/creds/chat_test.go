package creds

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestChatCommandClaude(t *testing.T) {
	a, _ := LookupAgent("claude")
	if !a.CanChat() {
		t.Fatal("claude has no chat")
	}
	first, err := a.ChatCommand(ChatTurn{NewID: "3f0c6b2e-8a51-4c1e-9d7a-0d9f1a2b3c4d"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(first, " ")
	for _, want := range []string{"claude -p --output-format stream-json --verbose", "--session-id 3f0c6b2e-8a51-4c1e-9d7a-0d9f1a2b3c4d", "--strict-mcp-config", "--allowedTools mcp__icaro", "--disallowedTools Bash,"} {
		if !strings.Contains(joined, want) {
			t.Errorf("first turn %q lacks %q", joined, want)
		}
	}
	if slices.Contains(first, "--resume") {
		t.Error("first turn resumes")
	}
	i := slices.Index(first, "--mcp-config")
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(first[i+1]), &cfg); err != nil || len(cfg.MCPServers) != 1 || cfg.MCPServers["icaro"].Command != "icaro" ||
		strings.Join(cfg.MCPServers["icaro"].Args, " ") != "mcp --socket "+MCPSocket {
		t.Errorf("mcp config %s: %v", first[i+1], err)
	}

	next, _ := a.ChatCommand(ChatTurn{ConversationID: "abc", NewID: "ignored"})
	if j := slices.Index(next, "--resume"); j < 0 || next[j+1] != "abc" || slices.Contains(next, "--session-id") {
		t.Errorf("resume turn %v", next)
	}
	bare, _ := a.ChatCommand(ChatTurn{})
	if slices.Contains(bare, "--session-id") || slices.Contains(bare, "--resume") {
		t.Errorf("turn without ids %v", bare)
	}
}

func TestChatCommandCodex(t *testing.T) {
	a, _ := LookupAgent("codex")
	first, _ := a.ChatCommand(ChatTurn{NewID: "ignored"})
	if got := strings.Join(first, " "); got != `codex exec --json --skip-git-repo-check --sandbox read-only -c approval_policy="never" -` {
		t.Errorf("first turn %q", got)
	}
	next, _ := a.ChatCommand(ChatTurn{ConversationID: "t1"})
	if got := strings.Join(next[:4], " "); got != "codex exec resume t1" || next[len(next)-1] != "-" {
		t.Errorf("resume turn %v", next)
	}
}

func TestChatCommandShell(t *testing.T) {
	a, _ := LookupAgent("")
	if a.CanChat() {
		t.Fatal("plain shell can chat")
	}
	if _, err := a.ChatCommand(ChatTurn{}); !errors.Is(err, ErrNoChat) {
		t.Fatalf("err = %v", err)
	}
}
