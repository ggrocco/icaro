// Package secrets is the daemon's secret store: named values sealed with
// the master key, each optionally bound to the hosts it may be sent to
// (03-security, "Integration credentials are host-bound"). Values never
// leave the daemon except into a container env (image-step secrets) or an
// outbound request to a bound host (integration credentials).
package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/store"
)

// ErrNotFound reports an unknown secret.
var ErrNotFound = errors.New("secret not found")

// ErrInvalid marks a rejected name, value or host binding: the caller's
// mistake, reported with its message.
var ErrInvalid = errors.New("invalid secret")

// ErrHostNotAllowed is returned when a value would be sent to a host
// outside its binding.
var ErrHostNotAllowed = errors.New("host is outside the secret's binding")

// Info describes a secret without its value. It is the only shape any API
// or MCP tool returns.
type Info struct {
	Name      string    `json:"name"`
	Hosts     []string  `json:"hosts"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store seals secrets in the database.
type Store struct {
	db  *store.Store
	key *crypto.Key
}

// New returns a Store.
func New(db *store.Store, key *crypto.Key) *Store { return &Store{db: db, key: key} }

var nameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)

// ValidName reports whether name is a valid secret name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Set stores value under name, bound to hosts (empty = usable only in
// container env, never in integration requests).
func (s *Store) Set(ctx context.Context, name, value string, hosts []string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w name %q", ErrInvalid, name)
	}
	if value == "" {
		return fmt.Errorf("%w: the value is empty", ErrInvalid)
	}
	norm := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if err := CheckHostPattern(h); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		norm = append(norm, h)
	}
	sort.Strings(norm)
	ct, err := s.key.Seal([]byte(value))
	if err != nil {
		return err
	}
	return s.db.PutSecret(ctx, &store.SecretRow{Name: name, ValueCT: ct, Hosts: norm})
}

// Resolve returns the plaintext value. Callers that send it over the
// network must use ResolveFor instead.
func (s *Store) Resolve(ctx context.Context, name string) (string, error) {
	r, err := s.db.GetSecret(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return "", err
	}
	plain, err := s.key.Open(r.ValueCT)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %q: %w", name, err)
	}
	return string(plain), nil
}

// ResolveFor returns the value only when host is inside its binding.
func (s *Store) ResolveFor(ctx context.Context, name, host string) (string, error) {
	r, err := s.db.GetSecret(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return "", err
	}
	if !HostAllowed(r.Hosts, host) {
		return "", fmt.Errorf("%w: %s may be sent to %v, not %q", ErrHostNotAllowed, name, r.Hosts, host)
	}
	plain, err := s.key.Open(r.ValueCT)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// CheckHostPattern validates a host binding or manifest host: a hostname,
// or "*." followed by a domain of at least two labels. "*.com" would
// send a credential to every .com host, so it is refused.
func CheckHostPattern(h string) error {
	if h == "" || strings.ContainsAny(h, "/: ") {
		return fmt.Errorf("host %q must be a hostname or *.domain", h)
	}
	rest, wildcard := strings.CutPrefix(h, "*.")
	if strings.Contains(rest, "*") {
		return fmt.Errorf("host %q: a wildcard is only allowed as a leading \"*.\"", h)
	}
	if wildcard && !strings.Contains(strings.Trim(rest, "."), ".") {
		return fmt.Errorf("wildcard host %q covers a whole top-level domain; name at least two labels after \"*.\", such as *.example.com", h)
	}
	return nil
}

// HostAllowed matches host against bindings ("api.github.com" exact,
// "*.slack.com" any subdomain). An empty binding allows nothing, and a
// top-level wildcard such as "*.com" (refused since, but possibly stored
// earlier) matches nothing.
func HostAllowed(bindings []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	for _, b := range bindings {
		if b == host {
			return true
		}
		if strings.HasPrefix(b, "*.") && strings.HasSuffix(host, b[1:]) && len(host) > len(b)-1 && CheckHostPattern(b) == nil {
			return true
		}
	}
	return false
}

// List returns names and bindings, never values.
func (s *Store) List(ctx context.Context) ([]Info, error) {
	rows, err := s.db.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(rows))
	for _, r := range rows {
		out = append(out, Info{Name: r.Name, Hosts: orEmpty(r.Hosts), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// Names lists secret names.
func (s *Store) Names(ctx context.Context) ([]string, error) {
	infos, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(infos))
	for i, in := range infos {
		out[i] = in.Name
	}
	return out, nil
}

// Delete removes a secret.
func (s *Store) Delete(ctx context.Context, name string) error {
	err := s.db.DeleteSecret(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return err
}

// Values returns every plaintext value (for log redaction only).
func (s *Store) Values(ctx context.Context) ([]string, error) {
	rows, err := s.db.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if plain, err := s.key.Open(r.ValueCT); err == nil {
			out = append(out, string(plain))
		}
	}
	return out, nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
