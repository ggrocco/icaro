package apiclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"icaro/internal/preview"
	"icaro/internal/store"
)

// Preview is the API view of a preview instance.
type Preview struct {
	store.PreviewInstance
	Curl string `json:"curl,omitempty"`
}

// ListPreviews returns every preview.
func (c *Client) ListPreviews(ctx context.Context) ([]Preview, error) {
	var out struct {
		Previews []Preview `json:"previews"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/previews", nil, &out)
	return out.Previews, err
}

// GetPreview returns one preview by id.
func (c *Client) GetPreview(ctx context.Context, id string) (*Preview, error) {
	var out Preview
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/previews/"+url.PathEscape(id), nil, &out)
	return &out, err
}

// PreviewUp brings a worktree's preview up and waits for it.
func (c *Client) PreviewUp(ctx context.Context, path string, force bool) (*Preview, error) {
	var out Preview
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/previews/up", map[string]any{"path": path, "force": force}, &out)
	return &out, err
}

// PreviewDown stops a preview and keeps its record.
func (c *Client) PreviewDown(ctx context.Context, path string) (*Preview, error) {
	var out Preview
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/previews/down", map[string]any{"path": path}, &out)
	return &out, err
}

// PreviewRemove tears a preview down and forgets it.
func (c *Client) PreviewRemove(ctx context.Context, path string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/previews/remove", map[string]any{"path": path}, nil)
}

// PreviewLint lints a worktree's compose file.
func (c *Client) PreviewLint(ctx context.Context, path string) (*preview.LintResult, error) {
	var out preview.LintResult
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/previews/lint", map[string]any{"path": path}, &out)
	return &out, err
}

// PreviewAllow records an explicit override of lint findings.
func (c *Client) PreviewAllow(ctx context.Context, path string, findings []string) (*preview.LintResult, error) {
	var out preview.LintResult
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/previews/allow", map[string]any{"path": path, "findings": findings, "confirm": true}, &out)
	return &out, err
}

// PreviewLogs returns the tail of a preview's compose logs.
func (c *Client) PreviewLogs(ctx context.Context, id string, tail int) ([]byte, error) {
	var raw []byte
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/previews/%s/logs?tail=%d", url.PathEscape(id), tail), "", nil, &raw)
	return raw, err
}
