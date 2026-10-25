// Package inject is the daemon side of credential injection (ADR 0028).
//
// An agent container on a profile that opts in gets a provider base URL
// that points at its egress proxy (for example
// ANTHROPIC_BASE_URL=http://icaro-egress:3129/anthropic) and a per-session
// placeholder key. The proxy relays those requests, unchanged, to the
// daemon over a Unix socket bind-mounted into the proxy container only.
// Here the daemon checks the placeholder (a capability token tied to one
// running session), narrows the request to the paths the agent CLIs need,
// drops every client credential header, adds the real key from the secret
// store and sends the request upstream over TLS with normal certificate
// verification. The response streams back with the key filtered out.
//
// The real key is never in any container: not in the agent's env, files,
// home or mounts, and not in the proxy container either.
package inject

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
)

// TokenPrefix starts every placeholder key, so it is recognisable in a
// container and can never be mistaken for a provider credential.
const TokenPrefix = "icaro-injected."

// ErrRefused is returned by an Authorizer for a token that is unknown,
// malformed, or belongs to a session that is not running on the profile.
var ErrRefused = errors.New("injection token refused")

// Authorizer maps a placeholder token to the running session it was issued
// to. It is implemented by the session manager.
type Authorizer interface {
	Authorize(ctx context.Context, profile, token string) (sessionID string, err error)
}

// Keys resolves a secret's value for a host it is bound to (the secret
// store's ResolveFor): an injected key must be bound to its provider's API
// host.
type Keys interface {
	ResolveFor(ctx context.Context, name, host string) (string, error)
}

// route is one allowed request: a method and a path, where a "*" segment
// matches one non-empty path segment.
type route struct{ method, path string }

// Provider is one upstream API the injector can call.
type Provider struct {
	Name string
	// Upstream is the API origin; requests go to Upstream + path.
	Upstream string
	// BaseSuffix is appended to the agent's base URL (the OpenAI SDKs
	// expect the /v1 in it, the Anthropic SDK does not).
	BaseSuffix string
	// BaseURLEnv and KeyEnv are the variables the agent CLI reads.
	BaseURLEnv, KeyEnv string
	routes             []route
	setKey             func(h http.Header, key string)
}

// Providers are the supported providers, keyed by the names
// profiles.<name>.inject uses. Only the endpoints the agent CLIs need for
// a conversation are allowed; whether that list is complete for current
// Claude Code and Codex releases is spike S4's open check.
var Providers = map[string]Provider{
	"anthropic": {
		Name: "anthropic", Upstream: "https://api.anthropic.com",
		BaseURLEnv: "ANTHROPIC_BASE_URL", KeyEnv: "ANTHROPIC_API_KEY",
		routes: []route{
			{http.MethodPost, "/v1/messages"},
			{http.MethodPost, "/v1/messages/count_tokens"},
			{http.MethodGet, "/v1/models"},
			{http.MethodGet, "/v1/models/*"},
		},
		setKey: func(h http.Header, key string) { h.Set("X-Api-Key", key) },
	},
	"openai": {
		Name: "openai", Upstream: "https://api.openai.com", BaseSuffix: "/v1",
		BaseURLEnv: "OPENAI_BASE_URL", KeyEnv: "OPENAI_API_KEY",
		routes: []route{
			{http.MethodPost, "/v1/responses"},
			{http.MethodPost, "/v1/chat/completions"},
			{http.MethodGet, "/v1/models"},
			{http.MethodGet, "/v1/models/*"},
		},
		setKey: func(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) },
	},
}

// Allowed reports whether method and path (the part after the provider
// prefix, as decoded by net/http) are on the provider's allowlist.
func (p Provider) Allowed(method, path string) bool {
	if strings.Contains(path, "//") || strings.Contains(path, "/.") {
		return false
	}
	segs := strings.Split(path, "/")
	for _, r := range p.routes {
		if r.method == method && match(strings.Split(r.path, "/"), segs) {
			return true
		}
	}
	return false
}

func match(pattern, segs []string) bool {
	if len(pattern) != len(segs) {
		return false
	}
	for i, p := range pattern {
		switch {
		case p == "*":
			if !validSegment(segs[i]) {
				return false
			}
		case p != segs[i]:
			return false
		}
	}
	return true
}

// validSegment admits the characters model ids use.
func validSegment(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._:@", c)
		if !ok {
			return false
		}
	}
	return true
}

// Env is what an agent container of a profile that injects providers gets:
// for each provider its base URL (base is the proxy's injection endpoint,
// e.g. http://icaro-egress:3129) and the session's placeholder token as
// its API key. Unknown providers are skipped (config validation refuses
// them). The result is sorted, so container specs are stable.
func Env(base string, providers map[string]string, token string) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		if _, ok := Providers[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	base = strings.TrimSuffix(base, "/")
	var env []string
	for _, name := range names {
		p := Providers[name]
		env = append(env, p.BaseURLEnv+"="+base+"/"+name+p.BaseSuffix, p.KeyEnv+"="+token)
	}
	return env
}
