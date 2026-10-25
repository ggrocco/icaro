package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/store"
)

// Lifetimes of browser sessions and one-time login codes (ADR 0023).
const (
	UISessionTTL   = 12 * time.Hour
	UILoginCodeTTL = 2 * time.Minute
)

// UIPrincipal is who a browser session belongs to.
type UIPrincipal struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewUILoginCode mints a single-use login code for `icaro ui`.
func (s *Service) NewUILoginCode(ctx context.Context, name, scope string) (string, error) {
	code, err := randomSecret()
	if err != nil {
		return "", err
	}
	code = "icl_" + code
	row := &store.UISessionRow{ID: crypto.HashToken(code), Kind: store.UILoginCode, Name: name, Scope: scope, ExpiresAt: store.Now().Add(UILoginCodeTTL)}
	return code, s.store.CreateUISession(ctx, row)
}

// UILogin exchanges an API token or a one-time login code for a browser
// session; it returns the cookie value.
func (s *Service) UILogin(ctx context.Context, secret string) (string, *UIPrincipal, error) {
	if secret == "" {
		return "", nil, ErrUnauthorized
	}
	var p UIPrincipal
	var tokenID string // empty for login codes, which no token backs
	if code, err := s.store.ConsumeUILoginCode(ctx, crypto.HashToken(secret)); err == nil {
		p = UIPrincipal{Name: code.Name, Scope: code.Scope}
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", nil, err
	} else {
		tok, err := s.Authenticate(ctx, secret)
		if err != nil {
			return "", nil, ErrUnauthorized
		}
		p, tokenID = UIPrincipal{Name: tok.Name, Scope: tok.Scope}, tok.ID
	}
	cookie, err := randomSecret()
	if err != nil {
		return "", nil, err
	}
	row := &store.UISessionRow{ID: crypto.HashToken(cookie), Kind: store.UISession, Name: p.Name, Scope: p.Scope, TokenID: tokenID, ExpiresAt: store.Now().Add(UISessionTTL)}
	if err := s.store.CreateUISession(ctx, row); err != nil {
		return "", nil, err
	}
	_, _ = s.store.PurgeUISessions(ctx)
	s.log.Info("ui login", "name", p.Name, "scope", p.Scope)
	return cookie, &p, nil
}

// UISession resolves a cookie value.
func (s *Service) UISession(ctx context.Context, cookie string) (*UIPrincipal, error) {
	if cookie == "" {
		return nil, ErrUnauthorized
	}
	r, err := s.store.GetUISession(ctx, crypto.HashToken(cookie), store.UISession)
	if err != nil {
		return nil, ErrUnauthorized
	}
	return &UIPrincipal{Name: r.Name, Scope: r.Scope}, nil
}

// UILogout ends a browser session.
func (s *Service) UILogout(ctx context.Context, cookie string) error {
	if cookie == "" {
		return nil
	}
	return s.store.DeleteUISession(ctx, crypto.HashToken(cookie))
}
