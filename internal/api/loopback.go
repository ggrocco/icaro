package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"icaro/internal/service"
	"icaro/internal/store"
)

// Trusted loopback UI (ADR 0033). With daemon.ui_trust_loopback the UI
// listener serves the browser without a sign-in: a request that carries no
// bearer token acts as LoopbackPrincipal. Three checks keep that from
// being reachable from a web page:
//
//   - the connection must come from a loopback address;
//   - the Host header must name a loopback host, so a DNS-rebinding page
//     (whose requests carry its own hostname) is refused;
//   - an Origin header, when present, must be the same loopback origin,
//     and every mutation must carry X-Icaro-CSRF, which a cross-site page
//     cannot send without a CORS preflight the daemon never answers.

// LoopbackPrincipal is who a trusted loopback UI request acts as: the
// local user, with the same scope as the Unix socket.
var LoopbackPrincipal = &store.APIToken{ID: "loopback", Name: "local", Scope: store.ScopeAdmin}

// TrustLoopbackConnContext marks connections from a loopback peer as
// trusted UI connections; use it as the UI listener's ConnContext when
// daemon.ui_trust_loopback is on.
func TrustLoopbackConnContext(ctx context.Context, c net.Conn) context.Context {
	ctx = ConnContext(ctx, c)
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && a.IP.IsLoopback() {
		return context.WithValue(ctx, loopbackKey, true)
	}
	return ctx
}

func trustedLoopback(ctx context.Context) bool {
	v, _ := ctx.Value(loopbackKey).(bool)
	return v
}

// IsLoopbackPrincipal reports whether a request was let in by the trusted
// loopback UI rather than a token or a session.
func IsLoopbackPrincipal(ctx context.Context) bool {
	t := TokenFrom(ctx)
	return t != nil && t.ID == LoopbackPrincipal.ID
}

// loopbackHost reports whether a Host header (or an Origin's host) names
// a loopback address: localhost or a loopback IP, with any port.
func loopbackHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// checkLoopbackRequest applies the Host, Origin and CSRF checks.
func checkLoopbackRequest(r *http.Request) error {
	if !loopbackHost(r.Host) {
		return fmt.Errorf("%w: host %q is not a loopback address", service.ErrForbidden, r.Host)
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return fmt.Errorf("%w: cross-origin request from %q", service.ErrForbidden, o)
		}
	}
	if !safeMethod(r.Method) && r.Header.Get(CSRFHeader) != "1" {
		return fmt.Errorf("%w: missing %s header", service.ErrForbidden, CSRFHeader)
	}
	return nil
}
