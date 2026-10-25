// Package egressproxy is the forward proxy that enforces a profile's
// mandatory egress allowlist (ADR 0005). It runs inside a helper container
// that sits on both the profile's internal network and the outside
// network; agent containers can only reach the outside through it.
//
// It speaks HTTP CONNECT (for TLS) and absolute-URI HTTP forwarding, and
// denies every host that is not allow-listed. It never inspects TLS.
package egressproxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Allowed matches host against the allowlist: exact names, or "*.domain"
// for any subdomain of domain.
func Allowed(allow []string, host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "":
		case a == h:
			return true
		case strings.HasPrefix(a, "*.") && strings.HasSuffix(h, a[1:]) && len(h) > len(a)-1:
			return true
		}
	}
	return false
}

// Proxy is the HTTP handler.
type Proxy struct {
	Allow []string
	Log   *slog.Logger
	// Dial is used for upstream connections (tests inject one).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	transport *http.Transport
}

// New returns a proxy for allow.
func New(allow []string, log *slog.Logger) *Proxy {
	if log == nil {
		log = slog.Default()
	}
	p := &Proxy{Allow: allow, Log: log}
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	p.Dial = d.DialContext
	p.transport = &http.Transport{
		Proxy:                 nil, // never chain through the environment
		DialContext:           func(ctx context.Context, n, a string) (net.Conn, error) { return p.Dial(ctx, n, a) },
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return p
}

func (p *Proxy) deny(w http.ResponseWriter, host string) {
	p.Log.Warn("egress denied", "host", host)
	http.Error(w, "icaro egress: "+host+" is not in this profile's egress allowlist", http.StatusForbidden)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "icaro egress proxy: absolute URL required", http.StatusBadRequest)
		return
	}
	if r.URL.Scheme != "http" {
		http.Error(w, "icaro egress proxy: only http:// may be forwarded; use CONNECT for TLS", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	if !Allowed(p.Allow, host) {
		p.deny(w, host)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	removeHopHeaders(out.Header)
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "icaro egress proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	removeHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "icaro egress proxy: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	if !Allowed(p.Allow, host) {
		p.deny(w, host)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	upstream, err := p.Dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		http.Error(w, "icaro egress proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if n := buf.Reader.Buffered(); n > 0 {
		pending := make([]byte, n)
		_, _ = buf.Read(pending)
		_, _ = upstream.Write(pending)
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
	<-done
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func removeHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// ListenAndServe runs the proxy on addr until ctx ends.
func ListenAndServe(ctx context.Context, addr string, p *Proxy) error {
	p.Log.Info("egress proxy listening", "addr", addr, "allow", strings.Join(p.Allow, ","))
	return Serve(ctx, addr, p)
}

// Serve runs h on addr until ctx ends.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 15 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-errCh:
		return err
	}
}
