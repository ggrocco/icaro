package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Root is one directory tree the Claude Code provider scans.
type Root struct {
	// Label names the source in output, e.g. "profile:claude" or "host".
	Label string
	Dir   string
	// Required makes a missing directory an unavailable row instead of
	// silently contributing nothing (used for explicit opt-ins).
	Required bool
}

// ClaudeCode parses Claude Code JSONL transcripts
// (<root>/**/.claude/projects/**/*.jsonl, or <root>/projects/** for a
// root that is itself a .claude directory).
type ClaudeCode struct {
	Roots []Root
}

// Name implements Provider.
func (c *ClaudeCode) Name() string { return "claude-code" }

type ccLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// Collect implements Provider.
func (c *ClaudeCode) Collect(ctx context.Context, since time.Time, loc *time.Location) ([]Record, error) {
	var out []Record
	for _, root := range c.Roots {
		recs, err := c.collectRoot(ctx, root, since, loc)
		if err != nil {
			out = append(out, Record{Provider: c.Name(), Source: root.Label, Confidence: ConfidenceUnavailable, Error: "source unavailable: " + err.Error()})
			continue
		}
		out = append(out, recs...)
	}
	return out, nil
}

func (c *ClaudeCode) collectRoot(ctx context.Context, root Root, since time.Time, loc *time.Location) ([]Record, error) {
	if _, err := os.Stat(root.Dir); err != nil {
		if errors.Is(err, os.ErrNotExist) && !root.Required {
			return nil, nil
		}
		return nil, err
	}
	type key struct{ day, model string }
	agg := map[key]*Record{}
	seen := map[string]bool{}
	err := filepath.WalkDir(root.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtrees are skipped, not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.HasSuffix(p, ".jsonl") || !inProjects(root.Dir, p) {
			return nil
		}
		return scanFile(p, func(l *ccLine) {
			if l.Type != "assistant" || l.Message == nil || l.Message.Usage == nil || l.Message.Model == "" {
				return
			}
			t, err := time.Parse(time.RFC3339Nano, l.Timestamp)
			if err != nil || t.Before(since) {
				return
			}
			// Claude Code writes one line per content block of a response;
			// they repeat the same usage, so count each response once.
			if id := l.Message.ID + "/" + l.RequestID; id != "/" {
				if seen[id] {
					return
				}
				seen[id] = true
			}
			if strings.HasPrefix(l.Message.Model, "<") { // e.g. "<synthetic>"
				return
			}
			k := key{day: t.In(loc).Format("2006-01-02"), model: l.Message.Model}
			r := agg[k]
			if r == nil {
				r = &Record{Provider: c.Name(), Source: root.Label, Day: k.day, Model: k.model, Confidence: ConfidenceHigh}
				agg[k] = r
			}
			u := l.Message.Usage
			r.InputTokens += u.InputTokens
			r.OutputTokens += u.OutputTokens
			r.CacheWriteTokens += u.CacheCreationTokens
			r.CacheReadTokens += u.CacheReadTokens
			r.Messages++
		})
	})
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(agg))
	for _, r := range agg {
		r.CostUSD = EstimateCost(*r)
		out = append(out, *r)
	}
	return out, nil
}

// inProjects keeps only transcripts under a Claude "projects" directory:
// <root>/**/.claude/projects/** (a profile home), or <root>/projects/**
// when root is itself a .claude directory.
func inProjects(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	rel = "/" + filepath.ToSlash(rel)
	return strings.HasPrefix(rel, "/projects/") || strings.Contains(rel, "/.claude/projects/") || strings.Contains(rel, "/.config/claude/projects/")
}

// scanFile feeds every well-formed line to fn; malformed lines are skipped
// because transcripts may be mid-write.
func scanFile(p string, fn func(*ccLine)) error {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var l ccLine
		if json.Unmarshal(line, &l) == nil {
			fn(&l)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

// ProfileRoots lists one root per profile home under homesDir.
func ProfileRoots(homesDir string) []Root {
	entries, err := os.ReadDir(homesDir)
	if err != nil {
		return nil
	}
	var out []Root
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, Root{Label: "profile:" + e.Name(), Dir: filepath.Join(homesDir, e.Name())})
		}
	}
	return out
}
