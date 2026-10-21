package logs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRedactor(t *testing.T) {
	r := NewRedactor([]string{"secret", "secret-token", "short", "secret", "tiny"})
	if len(r.secrets) != 2 {
		t.Fatalf("short and duplicate values must be ignored: %q", r.secrets)
	}
	for _, tc := range []struct{ in, want string }{
		// The longer secret wins, so nothing of it is left behind.
		{"a secret-token b", "a *** b"},
		{"secret and secret", "*** and ***"},
		{"nothing here, tiny", "nothing here, tiny"},
	} {
		if got := r.RedactString(tc.in); got != tc.want {
			t.Errorf("RedactString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	var nilR *Redactor
	if got := nilR.Redact([]byte("secret")); string(got) != "secret" {
		t.Fatalf("a nil redactor passes lines through: %q", got)
	}
}

func TestRedactStringLongestFirst(t *testing.T) {
	out := RedactString("key=abcdef-123456", []string{"abcdef", "abcdef-123456"})
	if out != "key=***" {
		t.Fatalf("a secret containing another must be fully redacted: %q", out)
	}
	if got := RedactValue(42, nil); got != 42 {
		t.Fatalf("non-string scalars pass through: %v", got)
	}
}

type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

func TestScrubberWriteErrors(t *testing.T) {
	boom := errors.New("disk full")
	s := NewScrubber(failWriter{boom}, nil)
	if n, err := s.Write([]byte("line\n")); !errors.Is(err, boom) || n != 0 {
		t.Fatalf("a failed write reports 0 bytes and the error: %d %v", n, err)
	}
	s = NewScrubber(failWriter{boom}, nil)
	_, _ = s.Write([]byte("partial"))
	if err := s.Flush(); !errors.Is(err, boom) {
		t.Fatalf("flush must surface the writer error: %v", err)
	}
	var out bytes.Buffer
	if err := NewScrubber(&out, nil).Flush(); err != nil || out.Len() != 0 {
		t.Fatalf("flushing an empty scrubber writes nothing: %v %q", err, out.String())
	}
}

func TestStoreAndTailErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Dir: blocker}).Open("run", 0); err == nil {
		t.Fatal("a log directory under a file must fail")
	}
	if _, err := Tail(filepath.Join(dir, "missing.log"), 10); !os.IsNotExist(err) {
		t.Fatalf("tail of a missing log: %v", err)
	}
	// A truncated tail without any newline is returned as is.
	p := filepath.Join(dir, "one.log")
	if err := os.WriteFile(p, []byte("abcdefghij"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Tail(p, 4); err != nil || string(got) != "ghij" {
		t.Fatalf("%q %v", got, err)
	}
	// A directory opens and stats but cannot be read.
	if info, err := os.Stat(dir); err == nil && info.Size() > 0 {
		if _, err := Tail(dir, 1<<20); err == nil {
			t.Fatal("reading a directory as a log must fail")
		}
	}
}
