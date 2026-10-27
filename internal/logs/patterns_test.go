package logs

import (
	"strings"
	"testing"
)

func TestRedactString(t *testing.T) {
	in := "token my-secret-value and ghp_abcdefghijklmnopqrstuvwxyz0123 xoxb-1234567890-abc glpat-abcdefghijklmnopqrstu Authorization: Bearer abcdefghijklmnop1234 short abc"
	out := RedactString(in, []string{"my-secret-value", "abc"})
	for _, leak := range []string{"my-secret-value", "ghp_", "xoxb-", "glpat-", "abcdefghijklmnop1234"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "Bearer ***") || !strings.Contains(out, "short abc") {
		t.Fatalf("over-redacted: %s", out)
	}
	for _, tok := range []string{"sk_" + "live_51HxQabcdefghijklmnop", "rk_" + "test_51HxQabcdefghijklmnop", "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----"} {
		if got := RedactString("k="+tok+" end", nil); got != "k=*** end" {
			t.Fatalf("%q: %q", tok, got)
		}
	}
	v := RedactValue(map[string]any{"a": []any{"x my-secret-value", 3}}, []string{"my-secret-value"})
	if v.(map[string]any)["a"].([]any)[0] != "x ***" {
		t.Fatalf("%v", v)
	}
}
