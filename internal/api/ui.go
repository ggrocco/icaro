package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/web"
)

// Browser session plumbing (ADR 0023).
const (
	// SessionCookie holds the browser session; HttpOnly and SameSite=Strict.
	SessionCookie = "icaro_session"
	// CSRFHeader must accompany cookie-authenticated mutations.
	CSRFHeader = "X-Icaro-CSRF"
)

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// UIOptions configure the UI routes.
type UIOptions struct {
	// Assets is the built UI; nil uses the embedded web/dist.
	Assets fs.FS
	// Open, when set, makes the daemon reachable from a browser and
	// returns the base URL (e.g. http://127.0.0.1:8787); `icaro ui` calls
	// it over the local socket.
	Open func() (string, error)
}

// RegisterUI mounts the web UI and its session endpoints.
func (s *Server) RegisterUI(o UIOptions) {
	assets := o.Assets
	if assets == nil {
		assets = web.FS()
	}
	m := s.mux
	m.HandleFunc("POST /api/v1/ui/login", s.handleUILogin)
	m.HandleFunc("POST /api/v1/ui/logout", s.handleUILogout)
	m.Handle("GET /api/v1/ui/me", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		t := TokenFrom(r.Context())
		writeJSON(w, http.StatusOK, service.UIPrincipal{Name: t.Name, Scope: t.Scope})
	}))
	// Minting a login link is for the local user only (the socket).
	m.HandleFunc("POST /api/v1/ui/link", func(w http.ResponseWriter, r *http.Request) {
		if !IsLocal(r.Context()) {
			s.writeError(w, service.ErrForbidden)
			return
		}
		base := ""
		if o.Open != nil {
			u, err := o.Open()
			if err != nil {
				s.writeError(w, err)
				return
			}
			base = u
		}
		code, err := s.svc.NewUILoginCode(r.Context(), "local", store.ScopeWrite)
		if err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"url": strings.TrimRight(base, "/") + "/ui/?token=" + code, "base_url": base,
			"expires_in_seconds": int(service.UILoginCodeTTL / time.Second),
		})
	})
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	files := http.FileServerFS(assets)
	m.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/ui")
		rel = strings.TrimPrefix(rel, "/")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		if rel != "" {
			if fi, err := fs.Stat(assets, rel); err == nil && !fi.IsDir() {
				if strings.HasPrefix(rel, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/" + rel
				files.ServeHTTP(w, r2)
				return
			}
		}
		// SPA routes use the hash, so anything else is the shell.
		b, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			readme, _ := fs.ReadFile(assets, "README.txt")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(readme)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
}

func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	// The CSRF header also guards login, so a cross-site form cannot
	// sign a browser into someone else's session.
	if r.Header.Get(CSRFHeader) != "1" {
		s.writeError(w, service.ErrForbidden)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeError(w, err)
		return
	}
	cookie, p, err := s.svc.UILogin(r.Context(), strings.TrimSpace(in.Token))
	if err != nil {
		s.writeError(w, err)
		return
	}
	// Secure is set under TLS; the default UI listener is plain HTTP on
	// loopback (ADR 0023), where a Secure cookie is not portable.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name: SessionCookie, Value: cookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil, MaxAge: int(service.UISessionTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(CSRFHeader) != "1" {
		s.writeError(w, service.ErrForbidden)
		return
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		_ = s.svc.UILogout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1}) //nolint:gosec // expiring the cookie
	w.WriteHeader(http.StatusNoContent)
}
