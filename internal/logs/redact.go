package logs

import (
	"bytes"
	"sort"
)

// Redactor replaces known secret values (at least minSecretLen bytes) in
// complete lines. Unlike Scrubber it does no buffering, for callers that
// already split their input into lines.
type Redactor struct {
	secrets [][]byte
}

// NewRedactor builds a redactor; short and duplicate values are ignored.
func NewRedactor(secrets []string) *Redactor {
	r := &Redactor{}
	seen := map[string]bool{}
	for _, s := range secrets {
		if len(s) >= minSecretLen && !seen[s] {
			seen[s] = true
			r.secrets = append(r.secrets, []byte(s))
		}
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	return r
}

// Redact returns line with every secret replaced by Redacted.
func (r *Redactor) Redact(line []byte) []byte {
	if r == nil {
		return line
	}
	for _, s := range r.secrets {
		if bytes.Contains(line, s) {
			line = bytes.ReplaceAll(line, s, []byte(Redacted))
		}
	}
	return line
}

// RedactString is Redact for strings.
func (r *Redactor) RedactString(s string) string { return string(r.Redact([]byte(s))) }
