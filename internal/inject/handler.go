package inject

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"icaro/internal/logs"
)

// DefaultMaxBody caps a request body. Provider requests carry the whole
// conversation, images and documents included, so the cap is generous;
// it bounds what one request can make the daemon hold, not the stream.
const DefaultMaxBody = 32 << 20

// Handler serves one profile's injection socket.
type Handler struct {
	Profile string
	// Secrets maps each provider the profile injects to the secret that
	// holds its key (profiles.<name>.inject).
	Secrets map[string]string
	Auth    Authorizer
	Keys    Keys
	Audit   Audit
	Log     *slog.Logger
	// Transport sends upstream requests; nil means NewTransport().
	Transport http.RoundTripper
	// MaxBody caps request bodies; 0 means DefaultMaxBody.
	MaxBody int64
	// Upstreams overrides a provider's upstream origin (tests). It must
	// still be https.
	Upstreams map[string]string
}

// NewTransport is the upstream transport: TLS with the system roots and
// normal certificate verification, the daemon's own proxy settings, no
// overall timeout (responses stream for minutes) but a bounded wait for
// response headers.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
	}
}

// requestHeaderDrop are client headers never forwarded upstream: every
// credential the client might send (the real one is added here), cookies,
// hop-by-hop and forwarding headers, and the organisation/project
// selectors, so the key is used only with its own defaults.
var requestHeaderDrop = []string{
	"Authorization", "X-Api-Key", "Proxy-Authorization", "Cookie", "Host",
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	"Openai-Organization", "Openai-Project", "Anthropic-Admin-Api-Key",
	"Accept-Encoding", // the transport negotiates and decodes, so the key filter sees plain text
}

// hopHeaders are connection-scoped headers (RFC 9110 section 7.6.1).
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// responseHeaderDrop are upstream headers never returned: cookies, and
// Content-Length because the key filter may change the body's length.
var responseHeaderDrop = []string{"Set-Cookie", "Set-Cookie2", "Content-Length"}

func dropHeaders(h http.Header, names []string) {
	for _, f := range h.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
	for _, k := range names {
		h.Del(k)
	}
}

// clientToken is the placeholder the agent CLI sent: x-api-key (Anthropic)
// or a bearer token (OpenAI, or Anthropic with ANTHROPIC_AUTH_TOKEN).
func clientToken(h http.Header) string {
	if t := h.Get("X-Api-Key"); t != "" {
		return t
	}
	if a := h.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func (h *Handler) log() *slog.Logger {
	if h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

// writeError answers in the shape both providers' SDKs read (error.message).
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "icaro_injection_error", "message": "icaro: " + msg},
	})
}

// countingReader caps and counts a request body; the transport reads it
// on its own goroutine, hence the atomics.
type countingReader struct {
	r    io.ReadCloser
	max  int64
	n    atomic.Int64
	over atomic.Bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if total := c.n.Add(int64(n)); total > c.max {
		c.over.Store(true)
		return 0, fmt.Errorf("request body over %d bytes", c.max)
	}
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ent := Entry{Profile: h.Profile, Method: r.Method, Path: r.URL.Path}
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	rest = "/" + rest
	ent.Provider = name
	prov, known := Providers[name]
	secret, configured := h.Secrets[name]
	if !known || !configured {
		h.log().Warn("injection refused: provider not injected for this profile", "profile", h.Profile, "path", r.URL.Path)
		writeError(w, http.StatusNotFound, fmt.Sprintf("profile %s does not inject %q", h.Profile, name))
		return
	}
	sid, err := h.Auth.Authorize(r.Context(), h.Profile, clientToken(r.Header))
	if err != nil {
		if errors.Is(err, ErrRefused) {
			h.log().Warn("injection refused: bad or stale session token", "profile", h.Profile, "provider", name, "path", r.URL.Path, "reason", err)
			writeError(w, http.StatusUnauthorized, "this session's credential token is not valid (is the session running?)")
			return
		}
		h.log().Error("injection: authorize", "profile", h.Profile, "err", err)
		writeError(w, http.StatusServiceUnavailable, "cannot check the session token now")
		return
	}
	ent.Session = sid
	var key string
	defer func() {
		ent.DurationMS = time.Since(start).Milliseconds()
		if ent.Error != "" {
			ent.Error = logs.RedactString(ent.Error, []string{key})
		}
		if h.Audit != nil {
			h.Audit.Record(ent)
		}
	}()
	if r.URL.RawPath != "" || !prov.Allowed(r.Method, rest) {
		ent.Status, ent.Error = http.StatusForbidden, "path not allowed"
		writeError(w, http.StatusForbidden, fmt.Sprintf("%s %s is not an allowed %s API call", r.Method, rest, name))
		return
	}
	maxBody := h.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}
	if r.ContentLength > maxBody {
		ent.Status, ent.Error = http.StatusRequestEntityTooLarge, "request body too large"
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body over %d bytes", maxBody))
		return
	}
	upstream := prov.Upstream
	if u, ok := h.Upstreams[name]; ok {
		upstream = u
	}
	base, err := url.Parse(upstream)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		ent.Status, ent.Error = http.StatusBadGateway, "upstream is not an https URL"
		writeError(w, http.StatusBadGateway, "upstream is not an https URL")
		return
	}
	key, err = h.Keys.ResolveFor(r.Context(), secret, base.Hostname())
	if err != nil || key == "" {
		ent.Status, ent.Error = http.StatusBadGateway, fmt.Sprintf("credential %s unavailable: %v", secret, err)
		h.log().Error("injection: credential unavailable", "profile", h.Profile, "provider", name, "secret", secret, "err", logs.RedactString(fmt.Sprint(err), nil))
		writeError(w, http.StatusBadGateway, fmt.Sprintf("the %s credential (secret %s, bound to %s) is unavailable; see icaro doctor", name, secret, base.Hostname()))
		return
	}

	body := &countingReader{r: r.Body, max: maxBody}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, (&url.URL{
		Scheme: base.Scheme, Host: base.Host, Path: strings.TrimSuffix(base.Path, "/") + rest, RawQuery: r.URL.RawQuery,
	}).String(), body)
	if err != nil {
		ent.Status, ent.Error = http.StatusBadRequest, err.Error()
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if r.ContentLength == 0 {
		out.Body = http.NoBody
	}
	out.ContentLength = r.ContentLength
	out.Header = r.Header.Clone()
	dropHeaders(out.Header, requestHeaderDrop)
	prov.setKey(out.Header, key)

	tr := h.Transport
	if tr == nil {
		tr = NewTransport()
	}
	resp, err := tr.RoundTrip(out)
	ent.RequestBytes = body.n.Load()
	if err != nil {
		if body.over.Load() {
			ent.Status, ent.Error = http.StatusRequestEntityTooLarge, "request body too large"
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body over %d bytes", maxBody))
			return
		}
		ent.Status, ent.Error = http.StatusBadGateway, "upstream: "+err.Error()
		h.log().Warn("injection: upstream failed", "profile", h.Profile, "session", sid, "err", logs.RedactString(err.Error(), []string{key}))
		writeError(w, http.StatusBadGateway, name+" API unreachable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	dropHeaders(resp.Header, responseHeaderDrop)
	for k, vs := range resp.Header {
		for _, v := range vs {
			if strings.Contains(v, key) {
				continue // never hand the key back, whatever the upstream echoes
			}
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	ent.Status = resp.StatusCode
	n, err := stream(w, resp.Body, key)
	ent.ResponseBytes = n
	if err != nil {
		ent.Error = "stream: " + err.Error()
	}
}

// stream copies an upstream body to w as it arrives (server-sent events
// included), flushing after every read and filtering the key out.
func stream(w http.ResponseWriter, body io.Reader, key string) (int64, error) {
	rc := http.NewResponseController(w)
	f := &keyFilter{w: w, key: []byte(key)}
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			total += int64(n)
			if _, err := f.Write(buf[:n]); err != nil {
				return total, err
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			err := f.Close()
			_ = rc.Flush()
			return total, err
		}
		if rerr != nil {
			_ = f.Close()
			return total, rerr
		}
	}
}
