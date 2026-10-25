package inject

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// MaxSocketPath is the portable limit for a Unix socket path (macOS
// sun_path is 104 bytes including the terminator).
const MaxSocketPath = 104

// Server serves one injection socket per profile that injects
// credentials. The socket's directory is bind-mounted into that profile's
// egress proxy container only; agent containers never see it.
type Server struct {
	// Profiles maps each injecting profile to its provider -> secret map.
	Profiles map[string]map[string]string
	// SocketPath returns the socket of a profile.
	SocketPath func(profile string) string
	Auth       Authorizer
	Keys       Keys
	Audit      Audit
	Log        *slog.Logger
	// Transport and Upstreams are passed to every Handler (tests).
	Transport http.RoundTripper
	Upstreams map[string]string
	// UID/GID own the sockets when the daemon runs as root, so the egress
	// proxy (which runs as that user) can connect to the 0600 socket.
	UID, GID int
}

// Name implements app.Component.
func (s *Server) Name() string { return "inject" }

func (s *Server) logger() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Listen opens the socket of profile: a fresh 0600 socket in a 0700
// directory.
func (s *Server) Listen(profile string) (net.Listener, error) {
	path := s.SocketPath(profile)
	if len(path) >= MaxSocketPath {
		return nil, fmt.Errorf("injection socket %q is %d bytes; Unix sockets need < %d (use a shorter ICARO_HOME)", path, len(path), MaxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	if err := handOver(os.Getuid() == 0, s.UID, s.GID, dir, path); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// handOver gives paths to uid:gid when the daemon runs as root (asRoot),
// so the non-root egress proxy can use the socket. The daemon created
// them just now in its own private run directory and no container runs
// on them yet, so nothing races the change.
func handOver(asRoot bool, uid, gid int, paths ...string) error {
	if !asRoot || uid == 0 {
		return nil
	}
	for _, p := range paths {
		if err := os.Lchown(p, uid, gid); err != nil { //nolint:gosec // see above
			return err
		}
	}
	return nil
}

// Handler returns the handler of profile.
func (s *Server) Handler(profile string) *Handler {
	return &Handler{
		Profile: profile, Secrets: s.Profiles[profile], Auth: s.Auth, Keys: s.Keys, Audit: s.Audit, Log: s.Log,
		Transport: s.Transport, Upstreams: s.Upstreams,
	}
}

// Run serves every profile's socket until ctx ends. A profile whose
// socket cannot be opened is logged and skipped: its sessions get an
// error from the proxy, the rest of the daemon keeps running.
func (s *Server) Run(ctx context.Context) error {
	names := make([]string, 0, len(s.Profiles))
	for name := range s.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	type served struct {
		hs   *http.Server
		path string
	}
	var servers []served
	for _, name := range names {
		ln, err := s.Listen(name)
		if err != nil {
			s.logger().Error("credential injection unavailable for profile", "profile", name, "err", err)
			continue
		}
		hs := &http.Server{Handler: s.Handler(name), ReadHeaderTimeout: 15 * time.Second}
		servers = append(servers, served{hs, s.SocketPath(name)})
		go func() { _ = hs.Serve(ln) }()
		s.logger().Info("credential injection listening", "profile", name, "socket", s.SocketPath(name))
	}
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, sv := range servers {
		_ = sv.hs.Shutdown(sctx)
		_ = os.Remove(sv.path)
	}
	return nil
}
