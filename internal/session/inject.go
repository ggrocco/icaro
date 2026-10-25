package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"icaro/internal/container"
	"icaro/internal/crypto"
	"icaro/internal/inject"
	"icaro/internal/store"
)

// The credential-injection half of the manager (ADR 0028). A session of a
// profile with `inject` gets, instead of a provider key, a capability
// token: inject.TokenPrefix + session id + "." + 32 random bytes. It only
// means something to the daemon's injector, reached through the profile's
// egress proxy; the store keeps its hash, each start issues a new one, and
// it stops working when the session stops, is orphaned or is removed.

// injectEnv issues the session's token and returns the provider variables
// for its container: base URLs on the proxy's relay and the token as the
// API key. A profile without inject gets nothing.
func (m *Manager) injectEnv(ctx context.Context, row *store.SessionRow, providers map[string]string, att container.Attachment) ([]string, error) {
	if len(providers) == 0 {
		return nil, nil
	}
	if att.Inject == "" {
		return nil, fmt.Errorf("profile %s injects credentials but its egress proxy has no injection relay", row.Profile)
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails (crypto/rand)
	token := inject.TokenPrefix + row.ID + "." + base64.RawURLEncoding.EncodeToString(raw)
	if err := m.o.Store.PutInjectToken(ctx, row.ID, crypto.HashToken(token)); err != nil {
		return nil, fmt.Errorf("injection token: %w", err)
	}
	return inject.Env(att.Inject, providers, token), nil
}

// revokeInject forgets a session's token; the session-status check in
// Authorize refuses it anyway, this keeps no stale hash around.
func (m *Manager) revokeInject(ctx context.Context, id string) {
	if err := m.o.Store.DeleteInjectToken(ctx, id); err != nil {
		m.o.Log.Warn("revoke injection token", "session", id, "err", err)
	}
}

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", inject.ErrRefused, fmt.Sprintf(format, args...))
}

// Authorize implements inject.Authorizer: token must be the current token
// of a running agent session of profile.
func (m *Manager) Authorize(ctx context.Context, profile, token string) (string, error) {
	rest, ok := strings.CutPrefix(token, inject.TokenPrefix)
	id, secret, ok2 := strings.Cut(rest, ".")
	if !ok || !ok2 || id == "" || secret == "" {
		return "", refused("not an injection token")
	}
	row, err := m.o.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", refused("no session %s", id)
	}
	if err != nil {
		return "", err
	}
	if row.Kind != KindAgent || row.Profile != profile {
		return "", refused("session %s is not an agent session of profile %s", id, profile)
	}
	if row.Status != StatusRunning {
		return "", refused("session %s is %s", id, row.Status)
	}
	want, err := m.o.Store.InjectTokenHash(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", refused("session %s has no injection token", id)
	}
	if err != nil {
		return "", err
	}
	if !crypto.ConstantTimeEqual(crypto.HashToken(token), want) {
		return "", refused("stale or forged token for session %s", id)
	}
	return id, nil
}

// ContainerIDs are the uid and gid session containers run as (and the
// egress proxy, when it relays credential injection).
func (m *Manager) ContainerIDs() (uid, gid int) { return m.o.UID, m.o.GID }

var _ inject.Authorizer = (*Manager)(nil)
