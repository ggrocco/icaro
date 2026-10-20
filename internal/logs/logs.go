// Package logs stores step logs on disk, scrubbing secrets as they are
// written.
package logs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// Redacted replaces secret values in logs.
const Redacted = "***"

// minSecretLen avoids redacting trivially short values that would mangle output.
const minSecretLen = 6

// Scrubber replaces known secret values in a byte stream. It is line
// buffered so a secret split across two writes is still caught when it
// lands within one line.
type Scrubber struct {
	mu      sync.Mutex
	w       io.Writer
	secrets [][]byte
	buf     bytes.Buffer
}

// NewScrubber wraps w; secrets shorter than minSecretLen are ignored.
func NewScrubber(w io.Writer, secrets []string) *Scrubber {
	s := &Scrubber{w: w}
	for _, sec := range secrets {
		if len(sec) >= minSecretLen {
			s.secrets = append(s.secrets, []byte(sec))
		}
	}
	// Longest first so a secret that contains another is fully redacted.
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
	return s
}

func (s *Scrubber) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	for {
		i := bytes.IndexByte(s.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := make([]byte, i+1)
		copy(line, s.buf.Bytes()[:i+1])
		s.buf.Next(i + 1)
		if _, err := s.w.Write(s.scrub(line)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes any trailing partial line.
func (s *Scrubber) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() == 0 {
		return nil
	}
	rest := s.scrub(append([]byte(nil), s.buf.Bytes()...))
	s.buf.Reset()
	_, err := s.w.Write(rest)
	return err
}

func (s *Scrubber) scrub(line []byte) []byte {
	for _, sec := range s.secrets {
		line = bytes.ReplaceAll(line, sec, []byte(Redacted))
	}
	return line
}

// Store lays out log files as <dir>/<run>/<idx>.log.
type Store struct {
	Dir string
}

// Path returns the log file path for a step.
func (s Store) Path(runID string, idx int) string {
	return filepath.Join(s.Dir, runID, strconv.Itoa(idx)+".log")
}

// Open returns an append-only file for the step log, creating directories.
func (s Store) Open(runID string, idx int) (*os.File, error) {
	p := s.Path(runID, idx)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// Tail returns up to maxBytes from the end of a log, starting at a line
// boundary when truncated.
func Tail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if st.Size() > maxBytes {
		start = st.Size() - maxBytes
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, nil
}
