package egressproxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
)

// InjectPrefixes are the paths the injection relay forwards (ADR 0028):
// one per provider the daemon can inject a credential for.
var InjectPrefixes = []string{"/anthropic/", "/openai/"}

// NewForwarder returns the injection relay: it forwards origin-form
// requests under InjectPrefixes, unchanged and streaming, to the daemon's
// injection socket, which this proxy container alone has mounted. It
// holds no credential and adds none: the daemon checks the session token,
// narrows the request and adds the real key.
func NewForwarder(socket string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "icaro-inject", "icaro-inject"
			pr.Out.Header.Del("Proxy-Authorization")
		},
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			// A local socket is cheap to dial, and a pooled connection would
			// outlive a daemon restart (which recreates the socket).
			DisableKeepAlives: true,
		},
		// Flush every write: model responses are server-sent events.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("injection relay failed", "path", r.URL.Path, "err", err)
			http.Error(w, "icaro egress: the daemon's credential injector is unreachable", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "icaro egress: the injection endpoint is not a proxy", http.StatusMethodNotAllowed)
			return
		}
		for _, p := range InjectPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				rp.ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "icaro egress: no credential injection at "+r.URL.Path, http.StatusNotFound)
	})
}

// ServeWithInjection runs the forward proxy on addr and the injection
// relay on injectAddr until ctx ends or either listener fails.
func ServeWithInjection(ctx context.Context, addr string, p *Proxy, injectAddr string, relay http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- ListenAndServe(ctx, addr, p) }()
	go func() {
		p.Log.Info("credential injection relay listening", "addr", injectAddr)
		errs <- Serve(ctx, injectAddr, relay)
	}()
	err := <-errs
	cancel()
	if err2 := <-errs; err == nil {
		err = err2
	}
	return err
}
