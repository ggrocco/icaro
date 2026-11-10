package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxSocketPath is the portable limit for a Unix socket path (macOS
// sun_path is 104 bytes including the terminator).
const MaxSocketPath = 104

// ErrSocketPath reports a socket path over MaxSocketPath.
var ErrSocketPath = errors.New("socket path too long")

// ServeUnix serves s over streamable HTTP on a new 0600 Unix socket at
// path, answering every request as caller: the socket's identity is the
// only scope its requests can have (ADR 0019). stop shuts the server down
// and removes the socket; cancelling ctx does the same.
func ServeUnix(ctx context.Context, s *Server, caller Caller, path string) (stop func(), err error) {
	if len(path) >= MaxSocketPath {
		return nil, fmt.Errorf("%w: %q is %d bytes; Unix sockets need < %d (use a shorter ICARO_HOME)", ErrSocketPath, path, len(path), MaxSocketPath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	h := s.HTTPHandler(func(*http.Request) (Caller, error) { return caller, nil })
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.Handle("/", h)
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()

	var once sync.Once
	done := make(chan struct{})
	stop = func() {
		once.Do(func() {
			close(done)
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = hs.Shutdown(sctx)
			_ = os.Remove(path)
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	return stop, nil
}
