package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"icaro/internal/integration"
)

// Integrations lists loaded integrations.
func (c *Client) Integrations(ctx context.Context) ([]integration.IntegrationInfo, error) {
	var out struct {
		Integrations []integration.IntegrationInfo `json:"integrations"`
	}
	return out.Integrations, c.Do(ctx, http.MethodGet, "/api/v1/integrations", nil, &out)
}

// Integration returns one integration with its manifest.
func (c *Client) Integration(ctx context.Context, name string) (*integration.IntegrationInfo, error) {
	var out integration.IntegrationInfo
	return &out, c.Do(ctx, http.MethodGet, "/api/v1/integrations/"+url.PathEscape(name), nil, &out)
}

// IntegrationStatus reports packs, load errors and missing secrets.
func (c *Client) IntegrationStatus(ctx context.Context) (*integration.Status, error) {
	var out integration.Status
	return &out, c.Do(ctx, http.MethodGet, "/api/v1/integrations/status", nil, &out)
}

// ReloadIntegrations asks the daemon to reload packs now.
func (c *Client) ReloadIntegrations(ctx context.Context) (*integration.Status, error) {
	var out integration.Status
	return &out, c.Do(ctx, http.MethodPost, "/api/v1/integrations/reload", nil, &out)
}

// EditResult is the outcome of a manifest edit.
type EditResult struct {
	Pack    string `json:"pack"`
	Commit  string `json:"commit"`
	Changed bool   `json:"changed"`
}

// EditManifest validates and commits a manifest to the local pack.
func (c *Client) EditManifest(ctx context.Context, name, yaml string) (*EditResult, error) {
	var out EditResult
	return &out, c.Do(ctx, http.MethodPut, "/api/v1/integrations/"+url.PathEscape(name)+"/manifest", map[string]string{"yaml": yaml}, &out)
}

// PlanCredential returns the default host binding for a credential.
func (c *Client) PlanCredential(ctx context.Context, integ, name string) (*integration.CredentialPlan, error) {
	var out integration.CredentialPlan
	q := url.Values{"integration": {integ}, "name": {name}}
	return &out, c.Do(ctx, http.MethodGet, "/api/v1/credentials/plan?"+q.Encode(), nil, &out)
}

// AddCredential stores a host-bound integration credential; nil hosts
// keeps the planned default binding.
func (c *Client) AddCredential(ctx context.Context, integ, name, value string, hosts []string) (*integration.CredentialPlan, error) {
	var out integration.CredentialPlan
	in := map[string]any{"integration": integ, "name": name, "value": value}
	if hosts != nil {
		in["hosts"] = hosts
	}
	return &out, c.Do(ctx, http.MethodPost, "/api/v1/credentials", in, &out)
}

// Credentials lists integration credentials (names and hosts only).
func (c *Client) Credentials(ctx context.Context) ([]integration.CredentialInfo, error) {
	var out struct {
		Credentials []integration.CredentialInfo `json:"credentials"`
	}
	return out.Credentials, c.Do(ctx, http.MethodGet, "/api/v1/credentials", nil, &out)
}

// DeleteCredential removes a credential.
func (c *Client) DeleteCredential(ctx context.Context, name string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/credentials/"+url.PathEscape(name), nil, nil)
}

// Deliveries lists webhook deliveries.
func (c *Client) Deliveries(ctx context.Context, integ, status string, limit int) ([]integration.DeliveryInfo, error) {
	q := url.Values{}
	if integ != "" {
		q.Set("integration", integ)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Deliveries []integration.DeliveryInfo `json:"deliveries"`
	}
	return out.Deliveries, c.Do(ctx, http.MethodGet, "/api/v1/deliveries?"+q.Encode(), nil, &out)
}

// Delivery returns one delivery with headers and body.
func (c *Client) Delivery(ctx context.Context, id string) (*integration.DeliveryInfo, error) {
	var out integration.DeliveryInfo
	return &out, c.Do(ctx, http.MethodGet, "/api/v1/deliveries/"+url.PathEscape(id), nil, &out)
}

// Redeliver re-runs a recorded delivery.
func (c *Client) Redeliver(ctx context.Context, id string) (*integration.Outcome, error) {
	var out integration.Outcome
	return &out, c.Do(ctx, http.MethodPost, "/api/v1/deliveries/"+url.PathEscape(id)+"/redeliver", nil, &out)
}

// WebhookToken is a generic webhook URL token.
type WebhookToken struct {
	Workflow  string `json:"workflow"`
	Token     string `json:"token"`
	Path      string `json:"path"`
	URL       string `json:"url,omitempty"`
	CreatedAt string `json:"created_at"`
}

// WebhookTokens lists generic webhook URL tokens.
func (c *Client) WebhookTokens(ctx context.Context) ([]WebhookToken, error) {
	var out struct {
		Tokens []WebhookToken `json:"tokens"`
	}
	return out.Tokens, c.Do(ctx, http.MethodGet, "/api/v1/webhook-tokens", nil, &out)
}

// RotateWebhookToken issues a new token for a workflow.
func (c *Client) RotateWebhookToken(ctx context.Context, wf string) (*WebhookToken, error) {
	var out WebhookToken
	return &out, c.Do(ctx, http.MethodPost, "/api/v1/webhook-tokens/"+url.PathEscape(wf)+"/rotate", nil, &out)
}

// Schedule is one cron trigger's persisted state.
type Schedule struct {
	Workflow    string `json:"workflow"`
	Trigger     int    `json:"trigger"`
	Cron        string `json:"cron"`
	NextAt      string `json:"next_at"`
	LastFiredAt string `json:"last_fired_at,omitempty"`
}

// Schedules lists cron triggers and their next fire times.
func (c *Client) Schedules(ctx context.Context) ([]Schedule, error) {
	var out struct {
		Schedules []Schedule `json:"schedules"`
	}
	return out.Schedules, c.Do(ctx, http.MethodGet, "/api/v1/schedules", nil, &out)
}
