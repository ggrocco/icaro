package api

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/creds"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/session"
	"icaro/internal/store"
)

// SessionDeps are what the session, profile and secret routes need.
type SessionDeps struct {
	Sessions *session.Manager
	Secrets  *secrets.Store
	Egress   container.Egress
	Images   interface {
		Resolve(ctx context.Context, ref string) (string, error)
	}
	Homes  *creds.Homes
	Config *config.Config
}

// ProfileInfo describes a profile for clients.
type ProfileInfo struct {
	Name   string   `json:"name"`
	Agent  string   `json:"agent"`
	Image  string   `json:"image"`
	Egress []string `json:"egress"`
}

// LoginInfo is what `icaro login` needs to run the agent's login with the
// profile seed mounted read-write.
type LoginInfo struct {
	Profile string               `json:"profile"`
	Image   string               `json:"image"`
	SeedDir string               `json:"seed_dir"`
	User    string               `json:"user"`
	Command []string             `json:"command"`
	Egress  container.Attachment `json:"egress"`
}

// RegisterSessions adds the session, project, profile and secret routes.
func (s *Server) RegisterSessions(d SessionDeps) {
	m := s.mux
	m.Handle("GET /api/v1/projects", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		ps, err := d.Sessions.Projects(r.Context())
		if err != nil {
			s.writeError(w, err)
			return
		}
		out := make([]map[string]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, map[string]string{"id": p.ID, "name": p.Name, "root_path": p.RootPath, "remote": p.Remote})
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": out})
	}))
	m.Handle("GET /api/v1/sessions", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		out, err := d.Sessions.List(r.Context(), session.ListFilter{Project: q.Get("project"), Kind: q.Get("kind"), Status: q.Get("status")})
		if err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		if out == nil {
			out = []*session.Session{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	}))
	create := func(ensure bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var in session.CreateRequest
			if err := readJSON(r, &in); err != nil {
				s.writeError(w, err)
				return
			}
			if in.RepoPath == "" {
				s.writeError(w, &service.BadRequest{Msg: "repo_path is required"})
				return
			}
			var out *session.Session
			var err error
			if ensure {
				out, err = d.Sessions.Ensure(r.Context(), in)
			} else {
				out, err = d.Sessions.Create(r.Context(), in)
			}
			if err != nil {
				s.writeError(w, sessionErr(err))
				return
			}
			writeJSON(w, http.StatusCreated, out)
		}
	}
	m.Handle("POST /api/v1/sessions", s.auth(store.ScopeWrite, create(false)))
	m.Handle("POST /api/v1/sessions/ensure", s.auth(store.ScopeWrite, create(true)))
	m.Handle("GET /api/v1/sessions/{id}", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		out, err := d.Sessions.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	m.Handle("GET /api/v1/sessions/{id}/exec", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		out, err := d.Sessions.Exec(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	m.Handle("POST /api/v1/sessions/{id}/stop", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		out, err := d.Sessions.Stop(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	m.Handle("POST /api/v1/sessions/{id}/start", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		out, err := d.Sessions.Start(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	m.Handle("POST /api/v1/sessions/{id}/push", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		var in session.PushRequest
		if err := readJSON(r, &in); err != nil {
			s.writeError(w, err)
			return
		}
		out, err := d.Sessions.Push(r.Context(), r.PathValue("id"), in)
		if err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	m.Handle("DELETE /api/v1/sessions/{id}", s.auth(store.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		keep := r.URL.Query().Get("keep_worktree") == "true"
		if err := d.Sessions.Remove(r.Context(), r.PathValue("id"), keep); err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.Handle("GET /api/v1/profiles", s.auth(store.ScopeRead, func(w http.ResponseWriter, _ *http.Request) {
		out := make([]ProfileInfo, 0, len(d.Config.Profiles))
		for name, p := range d.Config.Profiles {
			eg := p.Egress
			if eg == nil {
				eg = []string{}
			}
			out = append(out, ProfileInfo{Name: name, Agent: p.Agent, Image: p.Image, Egress: eg})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
	}))
	m.Handle("POST /api/v1/profiles/{name}/login", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		p, ok := d.Config.Profiles[name]
		if !ok {
			s.writeError(w, service.ErrNotFound)
			return
		}
		agent, err := creds.LookupAgent(p.Agent)
		if err != nil || len(agent.Login) == 0 {
			s.writeError(w, &service.BadRequest{Msg: "profile " + name + " has no agent to log in to"})
			return
		}
		image, err := d.Images.Resolve(r.Context(), p.Image)
		if err != nil {
			s.writeError(w, err)
			return
		}
		att, err := d.Egress.Ensure(r.Context(), name, p.Egress)
		if err != nil {
			s.writeError(w, err)
			return
		}
		seed, err := d.Homes.EnsureSeed(name)
		if err != nil {
			s.writeError(w, err)
			return
		}
		// The login rewrites the seed that live sessions may be using; the
		// lease keeps new ones from starting until DELETE ends it.
		if err := d.Sessions.BeginLogin(r.Context(), name, r.URL.Query().Get("force") == "true"); err != nil {
			s.writeError(w, sessionErr(err))
			return
		}
		writeJSON(w, http.StatusOK, LoginInfo{Profile: name, Image: image, SeedDir: seed, User: d.Sessions.User(), Command: agent.Login, Egress: att})
	}))
	// The login is over (done or failed): sessions of the profile may start.
	m.Handle("DELETE /api/v1/profiles/{name}/login", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		d.Sessions.EndLogin(r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}))

	m.Handle("GET /api/v1/secrets", s.auth(store.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		out, err := d.Secrets.List(r.Context())
		if err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
	}))
	m.Handle("PUT /api/v1/secrets/{name}", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Value string   `json:"value"`
			Hosts []string `json:"hosts"`
		}
		if err := readJSON(r, &in); err != nil {
			s.writeError(w, err)
			return
		}
		if err := d.Secrets.Set(r.Context(), r.PathValue("name"), in.Value, in.Hosts); err != nil {
			s.writeError(w, &service.BadRequest{Msg: err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	m.Handle("DELETE /api/v1/secrets/{name}", s.auth(store.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		if err := d.Secrets.Delete(r.Context(), r.PathValue("name")); err != nil {
			if errors.Is(err, secrets.ErrNotFound) {
				err = service.ErrNotFound
			}
			s.writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func sessionErr(err error) error {
	var inv *session.ErrInvalid
	var unavailable *session.ErrUnavailable
	var busy *session.ErrBusy
	var push *session.ErrPush
	switch {
	case errors.As(err, &push):
		return &service.Upstream{Msg: push.Msg}
	case errors.As(err, &busy):
		return &service.Conflict{Msg: busy.Msg}
	case errors.Is(err, session.ErrNotFound):
		return service.ErrNotFound
	case errors.As(err, &inv):
		return &service.BadRequest{Msg: inv.Msg}
	case errors.As(err, &unavailable):
		return &service.Unavailable{Msg: unavailable.Msg}
	}
	return err
}
