package inject

import (
	"bytes"
	"io"

	"icaro/internal/logs"
)

// keyFilter replaces every occurrence of key in a stream with
// logs.Redacted without buffering it: it only holds back the longest tail
// of what it has seen that could be the start of the key, which in
// practice is nothing (server-sent events end in a blank line, and keys
// start with a fixed prefix), so events are not delayed.
type keyFilter struct {
	w    io.Writer
	key  []byte
	held []byte
}

func (f *keyFilter) Write(p []byte) (int, error) {
	if len(f.key) == 0 {
		return f.w.Write(p)
	}
	buf := append(f.held, p...)
	buf = bytes.ReplaceAll(buf, f.key, []byte(logs.Redacted))
	keep := partialSuffix(buf, f.key)
	out := buf[:len(buf)-keep]
	f.held = append([]byte(nil), buf[len(buf)-keep:]...)
	if len(out) > 0 {
		if _, err := f.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close writes what is still held back: at the end of the stream it can
// no longer become the key.
func (f *keyFilter) Close() error {
	if len(f.held) == 0 {
		return nil
	}
	_, err := f.w.Write(f.held)
	f.held = nil
	return err
}

// partialSuffix is the length of the longest proper prefix of key that
// buf ends with.
func partialSuffix(buf, key []byte) int {
	n := len(key) - 1
	if len(buf) < n {
		n = len(buf)
	}
	for ; n > 0; n-- {
		if bytes.HasSuffix(buf, key[:n]) {
			return n
		}
	}
	return 0
}
