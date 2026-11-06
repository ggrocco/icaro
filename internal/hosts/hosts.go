// Package hosts signs this daemon in to remote Ícaro daemons and lists
// their workflows next to the local ones (ADR 0034).
//
// A remote host has an alias (the short name a reference uses, as in
// prod:deploy) and a label (its display name). The daemon may be signed in
// to any number of hosts at once; each sign-in is an API token of that
// host, sealed with the master key and only ever sent to that host's URL.
// The local daemon is always present as the host "local" and needs no
// sign-in.
//
// Listing asks every host at once: one goroutine per host sends its result
// on a channel, and the caller collects them, so one slow or unreachable
// host costs one timeout, not one per host, and never hides the others.
package hosts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"icaro/internal/apiclient/rest"
	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
)

// LocalAlias is the reserved alias of this daemon.
const LocalAlias = "local"

// DefaultTimeout bounds each host's answer to one listing or sign-in.
const DefaultTimeout = 10 * time.Second

// Errors the API maps to statuses.
var (
	// ErrInvalid is a bad alias, label, URL or token (400).
	ErrInvalid = errors.New("invalid")
	// ErrNotFound is an alias that is not signed in (404).
	ErrNotFound = errors.New("host not found")
	// ErrRefused is a host that answered but refused the token (400: the
	// caller's credential is wrong, not this daemon's session).
	ErrRefused = errors.New("refused")
	// ErrUnreachable is a host that did not answer (502).
	ErrUnreachable = errors.New("unreachable")
)

// kindError reads as its message alone and matches its kind with
// errors.Is, so messages do not start with the kind's name.
type kindError struct {
	kind error
	msg  string
}

func (e *kindError) Error() string        { return e.msg }
func (e *kindError) Is(target error) bool { return target == e.kind }

func errorf(kind error, format string, args ...any) error {
	return &kindError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

var aliasRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidAlias reports whether alias may name a host. Aliases are lowercase
// and contain no ':', so alias:workflow is unambiguous.
func ValidAlias(alias string) bool { return aliasRe.MatchString(alias) }

// Host is one host as the API shows it; it never carries the token.
type Host struct {
	Alias string `json:"alias"`
	Label string `json:"label"`
	// URL is the remote daemon's base URL; empty for the local host.
	URL   string `json:"url,omitempty"`
	Local bool   `json:"local,omitempty"`
	// Principal and Scope are who the token signed in as.
	Principal  string     `json:"principal,omitempty"`
	Scope      string     `json:"scope,omitempty"`
	SignedInAt *time.Time `json:"signed_in_at,omitempty"`
}

// Workflow is a workflow with the host it lives on.
type Workflow struct {
	*service.WorkflowInfo
	// Host is the alias and HostLabel the label of the workflow's host.
	Host      string `json:"host"`
	HostLabel string `json:"host_label"`
	// Ref is the workflow's host-qualified reference, alias:name.
	Ref string `json:"ref"`
	// UIURL opens the workflow in its host's web UI; empty for local
	// workflows, which the caller's own UI shows.
	UIURL string `json:"ui_url,omitempty"`
}

// HostResult is how one host answered a listing.
type HostResult struct {
	Host
	Workflows int    `json:"workflows"`
	Error     string `json:"error,omitempty"`
}

// Listing is the workflows of every host, local first, then remote hosts
// by alias, each host's workflows by name. A host that failed is in Hosts
// with its error and contributes no workflows.
type Listing struct {
	Workflows []Workflow   `json:"workflows"`
	Hosts     []HostResult `json:"hosts"`
}

// Remote is what the registry needs from a remote daemon.
type Remote interface {
	Me(ctx context.Context) (*service.UIPrincipal, error)
	ListWorkflows(ctx context.Context) ([]*service.WorkflowInfo, error)
}

// Options configure a Registry.
type Options struct {
	Store *store.Store
	Key   *crypto.Key
	// Local lists this daemon's workflows.
	Local func(ctx context.Context) ([]*service.WorkflowInfo, error)
	// LocalLabel names this daemon; empty uses the machine's hostname.
	LocalLabel string
	// Dial connects to a remote daemon; nil uses the HTTP API client.
	Dial    func(baseURL, token string) Remote
	Timeout time.Duration
	Log     *slog.Logger
}

// Registry holds the sign-ins. It keeps no state of its own: the store is
// the source of truth, so concurrent requests need no lock.
type Registry struct {
	store      *store.Store
	key        *crypto.Key
	local      func(ctx context.Context) ([]*service.WorkflowInfo, error)
	localLabel string
	dial       func(baseURL, token string) Remote
	timeout    time.Duration
	log        *slog.Logger
}

// New builds a Registry.
func New(o Options) *Registry {
	r := &Registry{store: o.Store, key: o.Key, local: o.Local, localLabel: o.LocalLabel, dial: o.Dial, timeout: o.Timeout, log: o.Log}
	if r.localLabel == "" {
		r.localLabel, _ = os.Hostname()
		if r.localLabel == "" {
			r.localLabel = "This machine"
		}
	}
	if r.dial == nil {
		r.dial = DialHTTP
	}
	if r.timeout <= 0 {
		r.timeout = DefaultTimeout
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r
}

// errRedirect refuses a redirect, so the token never leaves the URL it
// was given.
var errRedirect = errors.New("the host answered with a redirect; sign in with the URL it redirects to")

// DialHTTP is the default Dial: the API over HTTP(S), following no
// redirect.
func DialHTTP(baseURL, token string) Remote {
	return httpRemote{rest.NewWithHTTP(baseURL, token, &http.Client{
		Timeout:       DefaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	})}
}

// httpRemote is a remote daemon's API. It uses the rest transport, not
// package apiclient, which imports this package for its types.
type httpRemote struct{ c *rest.Client }

func (h httpRemote) Me(ctx context.Context) (*service.UIPrincipal, error) {
	var out service.UIPrincipal
	err := h.c.Do(ctx, http.MethodGet, "/api/v1/ui/me", nil, &out)
	return &out, err
}

func (h httpRemote) ListWorkflows(ctx context.Context) ([]*service.WorkflowInfo, error) {
	var out struct {
		Workflows []*service.WorkflowInfo `json:"workflows"`
	}
	err := h.c.Do(ctx, http.MethodGet, "/api/v1/workflows", nil, &out)
	return out.Workflows, err
}

// Login is a sign-in request.
type Login struct {
	Alias string `json:"alias"`
	Label string `json:"label"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

// NormalizeURL checks a remote base URL: http or https, a host, no
// credentials, query or fragment. The result has no trailing slash.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errorf(ErrInvalid, "url %q: %v", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errorf(ErrInvalid, "url %q must start with http:// or https://", raw)
	case u.Host == "":
		return "", errorf(ErrInvalid, "url %q has no host", raw)
	case u.User != nil:
		return "", errorf(ErrInvalid, "url %q must not carry credentials; pass the token separately", raw)
	case u.RawQuery != "" || u.Fragment != "":
		return "", errorf(ErrInvalid, "url %q must not have a query or fragment", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// Login verifies a token against the remote daemon and stores it under
// the alias, replacing an earlier sign-in with that alias.
func (r *Registry) Login(ctx context.Context, in Login) (*Host, error) {
	in.Alias = strings.TrimSpace(in.Alias)
	in.Label = strings.TrimSpace(in.Label)
	in.Token = strings.TrimSpace(in.Token)
	if !ValidAlias(in.Alias) {
		return nil, errorf(ErrInvalid, "alias %q must match %s", in.Alias, aliasRe)
	}
	if in.Alias == LocalAlias {
		return nil, errorf(ErrInvalid, "alias %q is this daemon, which needs no sign-in", LocalAlias)
	}
	if len(in.Label) > 100 {
		return nil, errorf(ErrInvalid, "label is longer than 100 characters")
	}
	if in.Label == "" {
		in.Label = in.Alias
	}
	if in.Token == "" {
		return nil, errorf(ErrInvalid, "token is required (create one on that host with `icaro token create`)")
	}
	base, err := NormalizeURL(in.URL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	me, err := r.dial(base, in.Token).Me(ctx)
	if err != nil {
		return nil, remoteError(base, err)
	}
	if me.Name == "" || me.Scope == "" {
		return nil, errorf(ErrUnreachable, "%s did not answer as an Ícaro daemon", base)
	}
	ct, err := r.key.Seal([]byte(in.Token))
	if err != nil {
		return nil, err
	}
	row := &store.RemoteHostRow{Alias: in.Alias, Label: in.Label, URL: base, TokenCT: ct, Principal: me.Name, Scope: me.Scope}
	if err := r.store.PutRemoteHost(ctx, row); err != nil {
		return nil, err
	}
	r.log.Info("signed in to host", "alias", in.Alias, "url", base, "principal", me.Name, "scope", me.Scope)
	row, err = r.store.GetRemoteHost(ctx, in.Alias)
	if err != nil {
		return nil, err
	}
	h := remoteHost(row)
	return &h, nil
}

// remoteError tells a host that refused the token from one that did not
// answer.
func remoteError(base string, err error) error {
	var ae *rest.Error
	if errors.As(err, &ae) {
		if ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden {
			return errorf(ErrRefused, "%s refused the token: %s", base, ae.Msg)
		}
		return errorf(ErrUnreachable, "%s answered %s", base, ae.Error())
	}
	return errorf(ErrUnreachable, "%s: %v", base, err)
}

// Logout forgets the token of a host.
func (r *Registry) Logout(ctx context.Context, alias string) error {
	if alias == LocalAlias {
		return errorf(ErrInvalid, "%q is this daemon, which has no sign-in", LocalAlias)
	}
	err := r.store.DeleteRemoteHost(ctx, alias)
	if errors.Is(err, store.ErrNotFound) {
		return errorf(ErrNotFound, "no host %q is signed in", alias)
	}
	if err == nil {
		r.log.Info("signed out of host", "alias", alias)
	}
	return err
}

func (r *Registry) localHost() Host {
	return Host{Alias: LocalAlias, Label: r.localLabel, Local: true}
}

func remoteHost(row *store.RemoteHostRow) Host {
	at := row.UpdatedAt
	return Host{Alias: row.Alias, Label: row.Label, URL: row.URL, Principal: row.Principal, Scope: row.Scope, SignedInAt: &at}
}

// List returns the local host, then every signed-in host by alias.
func (r *Registry) List(ctx context.Context) ([]Host, error) {
	rows, err := r.store.ListRemoteHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := []Host{r.localHost()}
	for _, row := range rows {
		out = append(out, remoteHost(row))
	}
	return out, nil
}

// fetch is one host's part of a listing.
type fetch struct {
	host Host
	list func(ctx context.Context) ([]*service.WorkflowInfo, error)
}

// answer is what a host's goroutine sends back.
type answer struct {
	index     int
	workflows []*service.WorkflowInfo
	err       error
}

// Workflows lists the workflows of every host, or of one host when alias
// is not empty. Every host is asked at the same time.
func (r *Registry) Workflows(ctx context.Context, alias string) (*Listing, error) {
	rows, err := r.store.ListRemoteHosts(ctx)
	if err != nil {
		return nil, err
	}
	var fetches []fetch
	if alias == "" || alias == LocalAlias {
		fetches = append(fetches, fetch{host: r.localHost(), list: r.local})
	}
	for _, row := range rows {
		if alias != "" && row.Alias != alias {
			continue
		}
		fetches = append(fetches, fetch{host: remoteHost(row), list: r.remoteLister(row)})
	}
	if len(fetches) == 0 {
		return nil, errorf(ErrNotFound, "no host %q is signed in", alias)
	}

	// Fan out: one goroutine per host, each sending exactly one answer on
	// a channel with room for all of them, so none blocks if the
	// collector returns early.
	answers := make(chan answer, len(fetches))
	for i, f := range fetches {
		go func() {
			hctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			ws, err := f.list(hctx)
			answers <- answer{index: i, workflows: ws, err: err}
		}()
	}
	got := make([]answer, len(fetches))
	for range fetches {
		a := <-answers
		got[a.index] = a
	}

	out := &Listing{Workflows: []Workflow{}, Hosts: make([]HostResult, 0, len(fetches))}
	for i, f := range fetches {
		a := got[i]
		res := HostResult{Host: f.host}
		if a.err != nil {
			res.Error = a.err.Error()
			r.log.Warn("list workflows", "host", f.host.Alias, "err", a.err)
			out.Hosts = append(out.Hosts, res)
			continue
		}
		ws := append([]*service.WorkflowInfo(nil), a.workflows...)
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].Name < ws[j].Name })
		for _, w := range ws {
			wf := Workflow{WorkflowInfo: w, Host: f.host.Alias, HostLabel: f.host.Label, Ref: f.host.Alias + ":" + w.Name}
			if !f.host.Local {
				wf.UIURL = f.host.URL + "/ui/#/workflows/" + url.PathEscape(w.Name)
			}
			out.Workflows = append(out.Workflows, wf)
			res.Workflows++
		}
		out.Hosts = append(out.Hosts, res)
	}
	return out, nil
}

// remoteLister opens a host's token and lists its workflows.
func (r *Registry) remoteLister(row *store.RemoteHostRow) func(context.Context) ([]*service.WorkflowInfo, error) {
	return func(ctx context.Context) ([]*service.WorkflowInfo, error) {
		tok, err := r.key.Open(row.TokenCT)
		if err != nil {
			return nil, fmt.Errorf("open the token of %s: %w (sign in again)", row.Alias, err)
		}
		ws, err := r.dial(row.URL, string(tok)).ListWorkflows(ctx)
		if err != nil {
			return nil, remoteError(row.URL, err)
		}
		return ws, nil
	}
}
