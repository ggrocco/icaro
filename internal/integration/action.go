package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"icaro/internal/config"
	"icaro/internal/expr"
	"icaro/internal/logs"
	"icaro/internal/workflow"
)

// CredentialStore is the slice of the secret store the executor uses.
// ResolveFor enforces the host binding.
type CredentialStore interface {
	ResolveFor(ctx context.Context, name, host string) (string, error)
	Values(ctx context.Context) ([]string, error)
}

// Executor runs `uses:` steps in-process (ADR 0007).
type Executor struct {
	Registry *Registry
	Secrets  CredentialStore
	// Settings returns the user's settings for an integration (config
	// integrations.<name>); a pack cannot set these.
	Settings func(integration string) config.IntegrationConfig
	Resolver Resolver
	Dial     DialFunc
	// TLSConfig overrides the client TLS settings (tests).
	TLSConfig   *tls.Config
	Timeout     time.Duration
	MaxResponse int64
}

var _ workflow.ActionExecutor = (*Executor)(nil)

// ActionError is a failed action call; its message is already redacted.
type ActionError struct {
	Ref    string
	Status int
	Msg    string
}

func (e *ActionError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Ref, e.Status, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Ref, e.Msg)
}

// Execute runs integration action ref with input and returns its outputs.
func (e *Executor) Execute(ctx context.Context, ref string, input map[string]any) (map[string]any, error) {
	known := e.knownSecrets(ctx)
	out, err := e.execute(ctx, ref, input, &known)
	if err != nil {
		var ae *ActionError
		if !errors.As(err, &ae) {
			ae = &ActionError{Ref: ref, Msg: err.Error()}
		}
		ae.Msg = logs.RedactString(ae.Msg, known)
		return nil, ae
	}
	red, _ := logs.RedactValue(out, known).(map[string]any)
	return red, nil
}

func (e *Executor) knownSecrets(ctx context.Context) []string {
	if e.Secrets == nil {
		return nil
	}
	vals, _ := e.Secrets.Values(ctx)
	return vals
}

func (e *Executor) execute(ctx context.Context, ref string, rawInput map[string]any, known *[]string) (map[string]any, error) {
	integ, actName, ok := strings.Cut(ref, ".")
	if !ok {
		return nil, fmt.Errorf("action reference %q must be <integration>.<action>", ref)
	}
	l, ok := e.Registry.Snapshot().Get(integ)
	if !ok {
		return nil, fmt.Errorf("unknown integration %q", integ)
	}
	m := l.Manifest
	act, ok := m.Actions[actName]
	if !ok {
		msg := fmt.Sprintf("integration %s has no action %q", integ, actName)
		if h := expr.Nearest(actName, sortedKeys(m.Actions)); h != "" {
			msg += fmt.Sprintf("; did you mean %q?", h)
		}
		return nil, errors.New(msg)
	}
	inputs, err := CheckInputs(act.Inputs, rawInput)
	if err != nil {
		return nil, err
	}
	settings := config.IntegrationConfig{}
	if e.Settings != nil {
		settings = e.Settings(integ)
	}
	cfgVals := map[string]any{}
	for k, f := range m.Config {
		cfgVals[k] = f.Default
	}
	if settings.BaseURL != "" {
		cfgVals["base_url"] = strings.TrimRight(settings.BaseURL, "/")
	}
	env := expr.Env{"input": inputs, "config": cfgVals}

	rq := act.Request
	rawURL, err := expr.RenderString(rq.URL, env)
	if err != nil {
		return nil, fmt.Errorf("render url: %w", err)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		if b, _ := cfgVals["base_url"].(string); b == "" && strings.Contains(rq.URL, "config.base_url") {
			return nil, fmt.Errorf("integration %s needs a base URL: set integrations.%s.base_url in the config", integ, integ)
		}
		return nil, fmt.Errorf("invalid request URL %q", rawURL)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && settings.AllowPrivate:
	default:
		return nil, fmt.Errorf("refusing %s URL %q: actions use https (plain http needs integrations.%s.allow_private)", u.Scheme, rawURL, integ)
	}
	if !hostDeclared(m.Hosts, host) && !configuredHost(settings.BaseURL, host) {
		return nil, fmt.Errorf("refusing request to %q: not in the manifest's hosts %v", host, m.Hosts)
	}
	q := u.Query()
	for _, k := range sortedKeys(rq.Query) {
		v, err := expr.RenderString(rq.Query[k], env)
		if err != nil {
			return nil, fmt.Errorf("render query %s: %w", k, err)
		}
		q.Set(k, v)
	}
	headers := http.Header{}
	for _, k := range sortedKeys(rq.Headers) {
		v, err := expr.RenderString(rq.Headers[k], env)
		if err != nil {
			return nil, fmt.Errorf("render header %s: %w", k, err)
		}
		headers.Set(k, v)
	}
	var body io.Reader
	if rq.Body != nil {
		b, ctype, err := renderBody(rq.Body, env)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
		if headers.Get("Content-Type") == "" && ctype != "" {
			headers.Set("Content-Type", ctype)
		}
	}
	if headers.Get("Accept") == "" {
		headers.Set("Accept", "application/json")
	}
	headers.Set("User-Agent", "icaro")

	if rq.Auth != "" {
		a := m.Auth[rq.Auth]
		if a.Kind != AuthNone {
			cred := integ + "." + a.Credential
			if e.Secrets == nil {
				return nil, errors.New("no secret store configured")
			}
			val, err := e.Secrets.ResolveFor(ctx, cred, host)
			if err != nil {
				return nil, fmt.Errorf("credential %s: %w (add it with `icaro credential add %s %s`)", cred, err, integ, a.Credential)
			}
			*known = append(*known, val)
			switch a.Kind {
			case AuthBearer:
				headers.Set("Authorization", "Bearer "+val)
			case AuthHeader:
				headers.Set(a.Header, a.Prefix+val)
			case AuthQuery:
				q.Set(a.Param, val)
			case AuthBasic:
				userpass := val
				if a.Username != "" {
					user, err := expr.RenderString(a.Username, env)
					if err != nil {
						return nil, err
					}
					userpass = user + ":" + val
				}
				headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(userpass)))
				*known = append(*known, base64.StdEncoding.EncodeToString([]byte(userpass)))
			}
		}
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, rq.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	resp, err := e.client(settings.AllowPrivate).Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	limit := e.MaxResponse
	if limit <= 0 {
		limit = 1 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, &ActionError{Ref: ref, Status: resp.StatusCode, Msg: fmt.Sprintf("response exceeds %d bytes", limit)}
	}
	respHeaders := map[string]any{}
	for k, v := range resp.Header {
		respHeaders[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	var respBody any = string(raw)
	if len(bytes.TrimSpace(raw)) > 0 {
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			respBody = parsed
		}
	}
	if !statusOK(resp.StatusCode, act.ExpectStatus) {
		return nil, &ActionError{Ref: ref, Status: resp.StatusCode, Msg: snippet(raw)}
	}
	env["response"] = map[string]any{"status": float64(resp.StatusCode), "headers": respHeaders, "body": respBody}
	if act.Success != "" {
		c, err := expr.ParseCondition(act.Success)
		if err != nil {
			return nil, err
		}
		ok, err := c.EvalBool(env)
		if err != nil || !ok {
			return nil, &ActionError{Ref: ref, Status: resp.StatusCode, Msg: "success condition failed: " + snippet(raw)}
		}
	}
	outputs := map[string]any{}
	for _, k := range sortedKeys(act.Outputs) {
		t, err := expr.ParseTemplate(act.Outputs[k])
		if err != nil {
			return nil, err
		}
		v, err := t.Eval(env)
		if err != nil {
			return nil, fmt.Errorf("output %s: %w", k, err)
		}
		outputs[k] = v
	}
	return outputs, nil
}

func (e *Executor) client(allowPrivate bool) *http.Client {
	res := e.Resolver
	if res == nil {
		res = net.DefaultResolver
	}
	dial := e.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = d.DialContext
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tr := &http.Transport{
		// No proxy: the SSRF guard must see the real destination.
		Proxy:               nil,
		DialContext:         guardedDial(res, dial, allowPrivate),
		TLSClientConfig:     e.TLSConfig,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// Redirects could leave the declared hosts; refuse them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func configuredHost(baseURL, host string) bool {
	if baseURL == "" {
		return false
	}
	u, err := url.Parse(baseURL)
	return err == nil && strings.EqualFold(u.Hostname(), host)
}

func statusOK(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 300
	}
	for _, c := range expect {
		if c == code {
			return true
		}
	}
	return false
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	if s == "" {
		s = "(empty body)"
	}
	return s
}

func renderBody(b *Body, env expr.Env) ([]byte, string, error) {
	switch {
	case b.JSON != nil:
		v, err := expr.EvalValue(b.JSON, env)
		if err != nil {
			return nil, "", fmt.Errorf("render body: %w", err)
		}
		raw, err := json.Marshal(v)
		ct := b.ContentType
		if ct == "" {
			ct = "application/json"
		}
		return raw, ct, err
	case b.Form != nil:
		form := url.Values{}
		for _, k := range sortedKeys(b.Form) {
			v, err := expr.RenderString(b.Form[k], env)
			if err != nil {
				return nil, "", fmt.Errorf("render form %s: %w", k, err)
			}
			form.Set(k, v)
		}
		return []byte(form.Encode()), "application/x-www-form-urlencoded", nil
	default:
		s, err := expr.RenderString(b.Raw, env)
		ct := b.ContentType
		if ct == "" {
			ct = "text/plain; charset=utf-8"
		}
		return []byte(s), ct, err
	}
}

// CheckInputs validates input against typed declarations: unknown names
// and missing required inputs are errors, defaults are applied, and
// strings are coerced to numbers/booleans when unambiguous (values that
// came through env interpolation).
func CheckInputs(decl map[string]Input, in map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var unknown []string
	for k := range in {
		if _, ok := decl[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		msg := fmt.Sprintf("unknown input(s): %s", strings.Join(unknown, ", "))
		if h := expr.Nearest(unknown[0], sortedKeys(decl)); h != "" {
			msg += fmt.Sprintf("; did you mean %q?", h)
		}
		return nil, errors.New(msg)
	}
	for _, k := range sortedKeys(decl) {
		d := decl[k]
		v, ok := in[k]
		if !ok || v == nil {
			if d.Default != nil {
				out[k] = d.Default
				continue
			}
			if d.Required {
				return nil, fmt.Errorf("missing required input %q", k)
			}
			continue
		}
		cv, err := coerce(d.Type, v)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", k, err)
		}
		out[k] = cv
	}
	return out, nil
}

func coerce(typ string, v any) (any, error) {
	if typeMatches(typ, v) {
		if n, ok := v.(int); ok {
			return float64(n), nil
		}
		return v, nil
	}
	if s, ok := v.(string); ok {
		switch typ {
		case "number":
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f, nil
			}
		case "boolean":
			if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
				return b, nil
			}
		case "array", "object":
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil && typeMatches(typ, parsed) {
				return parsed, nil
			}
		}
	}
	if typ == "string" {
		switch v.(type) {
		case float64, int, int64, bool:
			return expr.Stringify(v), nil
		}
	}
	return nil, fmt.Errorf("want %s, got %T", typ, v)
}
