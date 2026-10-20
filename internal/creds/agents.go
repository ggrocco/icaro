package creds

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MCPSocket is where a session's memory MCP socket appears in its container.
const MCPSocket = "/icaro/mcp.sock"

// MCPCommand is the stdio bridge the agent's MCP config runs (spike S3).
var MCPCommand = []string{"icaro", "mcp", "--socket", MCPSocket}

// Agent describes how one agent CLI runs, logs in, and is configured
// inside its profile home. Ícaro composes the CLI; it does not wrap it.
type Agent struct {
	Name string
	// Run is the interactive command; args from the user are appended.
	Run []string
	// Login seeds the profile home (run with the seed mounted read-write).
	Login []string
	// configure writes the MCP entry and preamble into the session layer.
	configure func(h HomeFiles, mcp bool, preamble string) error
}

// HomeFiles reads and writes one session's home.
type HomeFiles interface {
	Read(rel string) ([]byte, bool, error)
	Write(rel string, data []byte) error
}

// Agents is the catalog, keyed by profiles' `agent` field.
var Agents = map[string]Agent{
	"claude": {Name: "claude", Run: []string{"claude"}, Login: []string{"claude"}, configure: configureClaude},
	"codex":  {Name: "codex", Run: []string{"codex"}, Login: []string{"codex", "login"}, configure: configureCodex},
	"":       {Name: "shell", Run: nil, Login: nil, configure: nil},
}

// LookupAgent returns the agent for a profile's agent field.
func LookupAgent(name string) (Agent, error) {
	a, ok := Agents[name]
	if !ok {
		return Agent{}, fmt.Errorf("unknown agent %q (known: claude, codex, or empty for a plain shell)", name)
	}
	return a, nil
}

// Configure applies the agent's MCP entry and handoff preamble.
func (a Agent) Configure(h HomeFiles, mcp bool, preamble string) error {
	if a.configure == nil {
		return nil
	}
	return a.configure(h, mcp, preamble)
}

const preambleStart, preambleEnd = "<!-- icaro:preamble -->", "<!-- /icaro:preamble -->"

// withPreamble replaces (or appends) the Ícaro block in a markdown file.
func withPreamble(existing []byte, preamble string) []byte {
	s := string(existing)
	if i := strings.Index(s, preambleStart); i >= 0 {
		if j := strings.Index(s[i:], preambleEnd); j >= 0 {
			s = s[:i] + s[i+j+len(preambleEnd):]
		}
	}
	s = strings.TrimRight(s, "\n")
	if preamble == "" {
		return []byte(s + "\n")
	}
	if s != "" {
		s += "\n\n"
	}
	return []byte(s + preambleStart + "\n## Ícaro session\n\n" + preamble + "\n" + preambleEnd + "\n")
}

func configureClaude(h HomeFiles, mcp bool, preamble string) error {
	if mcp {
		raw, ok, err := h.Read(".claude.json")
		if err != nil {
			return err
		}
		cfg := map[string]any{}
		if ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return fmt.Errorf(".claude.json: %w", err)
			}
		}
		servers, _ := cfg["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		servers["icaro"] = map[string]any{"type": "stdio", "command": MCPCommand[0], "args": MCPCommand[1:]}
		cfg["mcpServers"] = servers
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		if err := h.Write(".claude.json", append(b, '\n')); err != nil {
			return err
		}
	}
	md, _, err := h.Read(".claude/CLAUDE.md")
	if err != nil {
		return err
	}
	return h.Write(".claude/CLAUDE.md", withPreamble(md, preamble))
}

func configureCodex(h HomeFiles, mcp bool, preamble string) error {
	if mcp {
		raw, _, err := h.Read(".codex/config.toml")
		if err != nil {
			return err
		}
		s := string(raw)
		if !strings.Contains(s, "[mcp_servers.icaro]") {
			if s != "" && !strings.HasSuffix(s, "\n") {
				s += "\n"
			}
			args := make([]string, len(MCPCommand)-1)
			for i, a := range MCPCommand[1:] {
				args[i] = fmt.Sprintf("%q", a)
			}
			s += fmt.Sprintf("\n[mcp_servers.icaro]\ncommand = %q\nargs = [%s]\n", MCPCommand[0], strings.Join(args, ", "))
			if err := h.Write(".codex/config.toml", []byte(s)); err != nil {
				return err
			}
		}
	}
	md, _, err := h.Read(".codex/AGENTS.md")
	if err != nil {
		return err
	}
	return h.Write(".codex/AGENTS.md", withPreamble(md, preamble))
}

// SessionFiles adapts Homes to HomeFiles for one session.
func (h *Homes) SessionFiles(profile, sessionID string) HomeFiles {
	return sessionFiles{h: h, profile: profile, id: sessionID}
}

type sessionFiles struct {
	h           *Homes
	profile, id string
}

func (s sessionFiles) Read(rel string) ([]byte, bool, error) { return s.h.Read(s.profile, s.id, rel) }
func (s sessionFiles) Write(rel string, data []byte) error {
	return s.h.Write(s.profile, s.id, rel, data)
}
