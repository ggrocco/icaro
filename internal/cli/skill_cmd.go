package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"icaro/skills"
)

func init() { register(newSkillCmd) }

// skillTarget says where an agent CLI keeps skills and MCP servers.
type skillTarget struct {
	skillsDir func(home string) string
	writeMCP  func(home, exe string) (string, error)
}

var skillTargets = map[string]skillTarget{
	"claude": {
		skillsDir: func(home string) string { return filepath.Join(home, ".claude", "skills") },
		writeMCP: func(home, exe string) (string, error) {
			p := filepath.Join(home, ".claude.json")
			return p, mergeJSONServer(p, "mcpServers", "icaro", map[string]any{"type": "stdio", "command": exe, "args": []string{"mcp"}})
		},
	},
	"codex": {
		skillsDir: func(home string) string { return filepath.Join(home, ".codex", "skills") },
		writeMCP: func(home, exe string) (string, error) {
			p := filepath.Join(home, ".codex", "config.toml")
			return p, appendTOMLServer(p, exe)
		},
	},
}

func newSkillCmd(_ *rootOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "skill", Short: "Install the agent skills shipped with icaro"}
	var target, home string
	var noMCP bool
	install := &cobra.Command{
		Use:   "install [NAME]",
		Short: "Write the icaro-workflows skill and the icaro MCP entry for Claude Code or Codex",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "icaro-workflows"
			if len(args) == 1 {
				name = args[0]
			}
			t, ok := skillTargets[target]
			if !ok {
				return fmt.Errorf("--target must be claude or codex")
			}
			if home == "" {
				h, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				home = h
			}
			sub, err := fs.Sub(skills.FS, name)
			if err != nil {
				return err
			}
			if _, err := fs.Stat(sub, "SKILL.md"); err != nil {
				return fmt.Errorf("unknown skill %q", name)
			}
			dest := filepath.Join(t.skillsDir(home), name)
			if err := copyFS(sub, dest); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "skill %s -> %s\n", name, dest)
			if noMCP {
				return nil
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			p, err := t.writeMCP(home, exe)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "mcp server \"icaro\" (%s mcp) -> %s\n", exe, p)
			return nil
		},
	}
	install.Flags().StringVar(&target, "target", "claude", "agent CLI: claude | codex")
	install.Flags().StringVar(&home, "home-dir", "", "home directory to install into (default: $HOME)")
	install.Flags().BoolVar(&noMCP, "no-mcp", false, "only write the skill, not the MCP entry")

	list := &cobra.Command{
		Use:   "list",
		Short: "List bundled skills",
		RunE: func(cmd *cobra.Command, _ []string) error {
			entries, err := fs.ReadDir(skills.FS, ".")
			if err != nil {
				return err
			}
			for _, e := range entries {
				if e.IsDir() {
					fmt.Fprintln(cmd.OutOrStdout(), e.Name())
				}
			}
			return nil
		},
	}
	cmd.AddCommand(install, list)
	return cmd
}

func copyFS(src fs.FS, dest string) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
}

// mergeJSONServer sets obj[section][name] = server in a JSON config file,
// preserving every other key.
func mergeJSONServer(path, section, name string, server map[string]any) error {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil && len(strings.TrimSpace(string(b))) > 0:
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	servers, _ := doc[section].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[name] = server
	doc[section] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

// appendTOMLServer adds an [mcp_servers.icaro] table to a Codex config
// unless one exists.
func appendTOMLServer(path, exe string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(b), "[mcp_servers.icaro]") {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	block := fmt.Sprintf("\n[mcp_servers.icaro]\ncommand = %q\nargs = [\"mcp\"]\n", exe)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(block)
	return err
}
