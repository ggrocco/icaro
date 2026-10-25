package logs

import (
	"regexp"
	"sort"
	"strings"
)

// TokenPatterns match well-known credential formats so they are redacted
// even when the value was never registered as a secret.
var TokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),                                             // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),                                           // GitHub fine-grained PAT
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),                                               // GitLab PAT
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`),                                          // Slack tokens
	regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{20,}\b`),                                         // OpenAI / Anthropic keys
	regexp.MustCompile(`\blin_(?:api|oauth)_[A-Za-z0-9]{20,}\b`),                                     // Linear keys
	regexp.MustCompile(`\bsntrys_[A-Za-z0-9+/=_-]{20,}\b`),                                           // Sentry org tokens
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`),                             // Authorization values
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                                       // AWS access key ids
	regexp.MustCompile(`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}\b`),                                   // Stripe secret and restricted keys
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?s:.*?)-----END [A-Z ]*PRIVATE KEY-----`), // PEM private keys
}

// RedactString replaces every known secret value (6+ characters) and every
// token pattern match in s with Redacted.
func RedactString(s string, secrets []string) string {
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		if len(v) >= minSecretLen {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, Redacted)
	}
	for _, re := range TokenPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			// Keep the scheme word of an Authorization value readable.
			if i := strings.IndexAny(m, " \t"); i > 0 && (strings.EqualFold(m[:i], "bearer") || strings.EqualFold(m[:i], "basic")) {
				return m[:i+1] + Redacted
			}
			return Redacted
		})
	}
	return s
}

// RedactValue applies RedactString to every string inside a JSON-like value.
func RedactValue(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		return RedactString(x, secrets)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = RedactValue(val, secrets)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = RedactValue(val, secrets)
		}
		return out
	}
	return v
}
