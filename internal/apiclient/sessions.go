package apiclient

import (
	"context"
	"net/http"
	"net/url"

	"icaro/internal/container"
	"icaro/internal/secrets"
	"icaro/internal/session"
)

// CreateSession creates an agent session.
func (c *Client) CreateSession(ctx context.Context, req session.CreateRequest) (*session.Session, error) {
	var out session.Session
	return &out, c.doJSON(ctx, http.MethodPost, "/api/v1/sessions", req, &out)
}

// EnsureSession returns a running session for the repo, creating one.
func (c *Client) EnsureSession(ctx context.Context, req session.CreateRequest) (*session.Session, error) {
	var out session.Session
	return &out, c.doJSON(ctx, http.MethodPost, "/api/v1/sessions/ensure", req, &out)
}

// ListSessions lists sessions.
func (c *Client) ListSessions(ctx context.Context, project, kind, status string) ([]*session.Session, error) {
	q := url.Values{}
	for k, v := range map[string]string{"project": project, "kind": kind, "status": status} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var out struct {
		Sessions []*session.Session `json:"sessions"`
	}
	path := "/api/v1/sessions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return out.Sessions, c.doJSON(ctx, http.MethodGet, path, nil, &out)
}

// GetSession fetches a session.
func (c *Client) GetSession(ctx context.Context, id string) (*session.Session, error) {
	var out session.Session
	return &out, c.doJSON(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(id), nil, &out)
}

// SessionExec returns the exec target of a running session.
func (c *Client) SessionExec(ctx context.Context, id string) (*session.ExecInfo, error) {
	var out session.ExecInfo
	return &out, c.doJSON(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(id)+"/exec", nil, &out)
}

// StopSession stops a session.
func (c *Client) StopSession(ctx context.Context, id string) (*session.Session, error) {
	var out session.Session
	return &out, c.doJSON(ctx, http.MethodPost, "/api/v1/sessions/"+url.PathEscape(id)+"/stop", nil, &out)
}

// StartSession restarts a stopped session.
func (c *Client) StartSession(ctx context.Context, id string) (*session.Session, error) {
	var out session.Session
	return &out, c.doJSON(ctx, http.MethodPost, "/api/v1/sessions/"+url.PathEscape(id)+"/start", nil, &out)
}

// PushSession pushes the session's own branch from the host.
func (c *Client) PushSession(ctx context.Context, id string, req session.PushRequest) (*session.PushResult, error) {
	var out session.PushResult
	return &out, c.doJSON(ctx, http.MethodPost, "/api/v1/sessions/"+url.PathEscape(id)+"/push", req, &out)
}

// RemoveSession removes a session (and its worktree unless keep).
func (c *Client) RemoveSession(ctx context.Context, id string, keepWorktree bool) error {
	path := "/api/v1/sessions/" + url.PathEscape(id)
	if keepWorktree {
		path += "?keep_worktree=true"
	}
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// Project is a registered repository.
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RootPath string `json:"root_path"`
	Remote   string `json:"remote"`
}

// ListProjects lists projects.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out struct {
		Projects []Project `json:"projects"`
	}
	return out.Projects, c.doJSON(ctx, http.MethodGet, "/api/v1/projects", nil, &out)
}

// Profile describes a profile.
type Profile struct {
	Name   string   `json:"name"`
	Agent  string   `json:"agent"`
	Image  string   `json:"image"`
	Egress []string `json:"egress"`
}

// ListProfiles lists profiles.
func (c *Client) ListProfiles(ctx context.Context) ([]Profile, error) {
	var out struct {
		Profiles []Profile `json:"profiles"`
	}
	return out.Profiles, c.doJSON(ctx, http.MethodGet, "/api/v1/profiles", nil, &out)
}

// LoginInfo mirrors api.LoginInfo.
type LoginInfo struct {
	Profile string               `json:"profile"`
	Image   string               `json:"image"`
	SeedDir string               `json:"seed_dir"`
	User    string               `json:"user"`
	Command []string             `json:"command"`
	Egress  container.Attachment `json:"egress"`
}

// PrepareLogin provisions what `icaro login` needs for a profile. The
// daemon refuses while sessions of the profile use its seed, unless force.
func (c *Client) PrepareLogin(ctx context.Context, profile string, force bool) (*LoginInfo, error) {
	var out LoginInfo
	path := "/api/v1/profiles/" + url.PathEscape(profile) + "/login"
	if force {
		path += "?force=true"
	}
	return &out, c.doJSON(ctx, http.MethodPost, path, nil, &out)
}

// ListSecrets lists secret names and bindings (never values).
func (c *Client) ListSecrets(ctx context.Context) ([]secrets.Info, error) {
	var out struct {
		Secrets []secrets.Info `json:"secrets"`
	}
	return out.Secrets, c.doJSON(ctx, http.MethodGet, "/api/v1/secrets", nil, &out)
}

// SetSecret stores a secret bound to hosts.
func (c *Client) SetSecret(ctx context.Context, name, value string, hosts []string) error {
	return c.doJSON(ctx, http.MethodPut, "/api/v1/secrets/"+url.PathEscape(name), map[string]any{"value": value, "hosts": hosts}, nil)
}

// DeleteSecret removes a secret.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/secrets/"+url.PathEscape(name), nil, nil)
}
