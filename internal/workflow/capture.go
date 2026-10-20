package workflow

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Output capture grammar for image steps (ADR 0013). Each value of a
// step's `outputs` map names where the output comes from:
//
//	json:<path>       a value inside /icaro/output.json; <path> is dotted
//	                  keys with numeric array indexes (json:pr.url,
//	                  json:items.0.id, json:items[0].id); json:$ is the
//	                  whole document
//	stdout            the step's whole stdout, trimmed (capped at 1 MiB)
//	stdout:last-line  the last non-empty stdout line, trimmed
//	file:<path>       a file under /workspace (relative, or absolute under
//	                  /workspace), trimmed (capped at 1 MiB)
//
// JSON captures keep their type; the others are strings.

// Capture kinds.
const (
	CaptureJSON         = "json"
	CaptureStdout       = "stdout"
	CaptureStdoutLast   = "stdout:last-line"
	CaptureFile         = "file"
	captureWholeJSONDoc = "$"
)

// Capture is a parsed output source.
type Capture struct {
	Kind string
	// JSONPath holds the key/index segments for json captures (empty =
	// whole document).
	JSONPath []string
	// File is the path relative to /workspace for file captures.
	File string
}

// CaptureForms lists the accepted forms for error hints.
const CaptureForms = "json:<path>, stdout, stdout:last-line, file:<path under /workspace>"

// ParseCapture parses an outputs value.
func ParseCapture(s string) (Capture, error) {
	switch {
	case s == CaptureStdout:
		return Capture{Kind: CaptureStdout}, nil
	case s == CaptureStdoutLast:
		return Capture{Kind: CaptureStdoutLast}, nil
	case strings.HasPrefix(s, "json:"):
		p := strings.TrimSpace(strings.TrimPrefix(s, "json:"))
		if p == "" {
			return Capture{}, errors.New("json: needs a path (use json:$ for the whole output.json)")
		}
		if p == captureWholeJSONDoc {
			return Capture{Kind: CaptureJSON}, nil
		}
		p = strings.TrimPrefix(p, "$.")
		p = strings.ReplaceAll(strings.ReplaceAll(p, "[", "."), "]", "")
		segs := strings.Split(p, ".")
		for _, seg := range segs {
			if seg == "" {
				return Capture{}, fmt.Errorf("json path %q has an empty segment", s)
			}
		}
		return Capture{Kind: CaptureJSON, JSONPath: segs}, nil
	case strings.HasPrefix(s, "file:"):
		p := strings.TrimSpace(strings.TrimPrefix(s, "file:"))
		if strings.HasPrefix(p, "/") {
			if p != "/workspace" && !strings.HasPrefix(p, "/workspace/") {
				return Capture{}, fmt.Errorf("file capture %q must be under /workspace", p)
			}
			p = strings.TrimPrefix(strings.TrimPrefix(p, "/workspace"), "/")
		}
		clean := path.Clean(p)
		if p == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return Capture{}, fmt.Errorf("file capture %q must name a file inside /workspace", s)
		}
		return Capture{Kind: CaptureFile, File: clean}, nil
	}
	return Capture{}, fmt.Errorf("unknown output source %q", s)
}

// FromJSON extracts a json capture from a decoded output.json.
func (c Capture) FromJSON(doc any) (any, error) {
	cur := doc
	for i, seg := range c.JSONPath {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return nil, fmt.Errorf("output.json has no %s", strings.Join(c.JSONPath[:i+1], "."))
			}
			cur = next
		case []any:
			n, err := strconv.Atoi(seg)
			if err != nil || n < 0 || n >= len(v) {
				return nil, fmt.Errorf("output.json has no %s", strings.Join(c.JSONPath[:i+1], "."))
			}
			cur = v[n]
		default:
			return nil, fmt.Errorf("output.json: %s is not an object or array", strings.Join(c.JSONPath[:i], "."))
		}
	}
	return cur, nil
}

// LastLine returns the last non-empty line of s, trimmed.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n\t "), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
