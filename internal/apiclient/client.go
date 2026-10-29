// Package apiclient is the Go client for the icaro HTTP API, used by the
// CLI and by the stdio MCP server. Its transport is package rest.
package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"icaro/internal/apiclient/rest"
	"icaro/internal/service"
	"icaro/internal/usage"
	"icaro/internal/workflow"
)

// Client talks to an icaro server: the rest transport plus typed calls.
type Client struct {
	*rest.Client
}

// New creates a client.
func New(baseURL, token string) *Client { return &Client{rest.New(baseURL, token)} }

// NewWithHTTP creates a client over a custom transport (e.g. the daemon's
// Unix socket).
func NewWithHTTP(baseURL, token string, hc *http.Client) *Client {
	return &Client{rest.NewWithHTTP(baseURL, token, hc)}
}

// Error is an API error response.
type Error = rest.Error

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return rest.IsNotFound(err) }

func (c *Client) do(ctx context.Context, method, path string, contentType string, body io.Reader, out any) error {
	return c.Send(ctx, method, path, contentType, body, out)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	return c.Do(ctx, method, path, in, out)
}

// Schema returns the workflow JSON Schema.
func (c *Client) Schema(ctx context.Context) ([]byte, error) {
	var raw []byte
	err := c.do(ctx, http.MethodGet, "/api/v1/schema/workflow", "", nil, &raw)
	return raw, err
}

// ValidateResult is the validate endpoint response.
type ValidateResult struct {
	Valid  bool            `json:"valid"`
	Name   string          `json:"name,omitempty"`
	Issues workflow.Issues `json:"issues"`
}

// Validate validates YAML without saving.
func (c *Client) Validate(ctx context.Context, yaml string) (*ValidateResult, error) {
	var out ValidateResult
	err := c.do(ctx, http.MethodPost, "/api/v1/workflows/validate", "application/x-yaml", strings.NewReader(yaml), &out)
	return &out, err
}

// ApplyWorkflow creates or updates a workflow.
func (c *Client) ApplyWorkflow(ctx context.Context, yaml string) (*service.WorkflowInfo, error) {
	var out service.WorkflowInfo
	err := c.do(ctx, http.MethodPost, "/api/v1/workflows", "application/x-yaml", strings.NewReader(yaml), &out)
	return &out, err
}

// ListWorkflows lists workflows.
func (c *Client) ListWorkflows(ctx context.Context) ([]*service.WorkflowInfo, error) {
	var out struct {
		Workflows []*service.WorkflowInfo `json:"workflows"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/workflows", nil, &out)
	return out.Workflows, err
}

// GetWorkflow fetches a workflow including YAML.
func (c *Client) GetWorkflow(ctx context.Context, name string) (*service.WorkflowInfo, error) {
	var out service.WorkflowInfo
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/workflows/"+url.PathEscape(name), nil, &out)
	return &out, err
}

// DeleteWorkflow deletes a workflow.
func (c *Client) DeleteWorkflow(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/workflows/"+url.PathEscape(name), nil, nil)
}

// SetWorkflowEnabled enables or disables a workflow.
func (c *Client) SetWorkflowEnabled(ctx context.Context, name string, enabled bool) (*service.WorkflowInfo, error) {
	action := "disable"
	if enabled {
		action = "enable"
	}
	var out service.WorkflowInfo
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/workflows/"+url.PathEscape(name)+"/"+action, nil, &out)
	return &out, err
}

// ListVersions lists workflow versions.
func (c *Client) ListVersions(ctx context.Context, name string) ([]service.VersionInfo, error) {
	var out struct {
		Versions []service.VersionInfo `json:"versions"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/workflows/"+url.PathEscape(name)+"/versions", nil, &out)
	return out.Versions, err
}

// GetVersion fetches one version with YAML.
func (c *Client) GetVersion(ctx context.Context, name string, version int) (*service.VersionInfo, error) {
	var out service.VersionInfo
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/workflows/"+url.PathEscape(name)+"/versions/"+strconv.Itoa(version), nil, &out)
	return &out, err
}

// Run starts a manual run with a trigger payload.
func (c *Client) Run(ctx context.Context, name string, trigger map[string]any) (*service.RunInfo, error) {
	var out service.RunInfo
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/workflows/"+url.PathEscape(name)+"/runs", map[string]any{"trigger": trigger}, &out)
	return &out, err
}

// GetRun fetches a run with steps.
func (c *Client) GetRun(ctx context.Context, id string) (*service.RunInfo, error) {
	var out service.RunInfo
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id), nil, &out)
	return &out, err
}

// ListRuns lists runs.
func (c *Client) ListRuns(ctx context.Context, workflowName, status string, limit int) ([]*service.RunInfo, error) {
	q := url.Values{}
	if workflowName != "" {
		q.Set("workflow", workflowName)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Runs []*service.RunInfo `json:"runs"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/runs?"+q.Encode(), nil, &out)
	return out.Runs, err
}

// Rerun starts a run again on the same workflow version and trigger.
func (c *Client) Rerun(ctx context.Context, id string) (*service.RunInfo, error) {
	var out service.RunInfo
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(id)+"/rerun", nil, &out)
	return &out, err
}

// CancelRun requests cancellation.
func (c *Client) CancelRun(ctx context.Context, id string) (*service.RunInfo, error) {
	var out service.RunInfo
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(id)+"/cancel", nil, &out)
	return &out, err
}

// StepLogs fetches a step's log: attempt 0 is the latest; tailBytes > 0
// returns only the end.
func (c *Client) StepLogs(ctx context.Context, id, step string, attempt, tailBytes int) ([]byte, error) {
	q := url.Values{}
	if attempt > 0 {
		q.Set("attempt", strconv.Itoa(attempt))
	}
	if tailBytes > 0 {
		q.Set("tail", strconv.Itoa(tailBytes))
	}
	path := fmt.Sprintf("/api/v1/runs/%s/steps/%s/logs?%s", url.PathEscape(id), url.PathEscape(step), q.Encode())
	var raw []byte
	err := c.do(ctx, http.MethodGet, path, "", nil, &raw)
	return raw, err
}

// Event is one Server-Sent Event from a run stream.
type Event struct {
	Name string          // snapshot, step, log, resync, end
	Data json.RawMessage // RunInfo for snapshot/end, events.Event otherwise
}

// Events follows a run's SSE stream, calling fn per event until the run
// ends (event "end"), fn returns false, or ctx ends. withLogs includes log
// lines.
func (c *Client) Events(ctx context.Context, id string, withLogs bool, fn func(Event) bool) error {
	path := "/api/v1/runs/" + url.PathEscape(id) + "/events"
	if withLogs {
		path += "?logs=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "text/event-stream")
	hc := *c.HTTP
	hc.Timeout = 0 // streams outlive request timeouts
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return rest.DecodeError(resp.StatusCode, data)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var ev Event
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Name != "" {
				if !fn(ev) || ev.Name == "end" {
					return nil
				}
			}
			ev = Event{}
		case strings.HasPrefix(line, "event: "):
			ev.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

// WaitRun polls until the run reaches a terminal state or ctx ends.
func (c *Client) WaitRun(ctx context.Context, id string, interval time.Duration) (*service.RunInfo, error) {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		run, err := c.GetRun(ctx, id)
		if err != nil {
			return nil, err
		}
		if service.Terminal(run.Status) {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Usage returns the usage report for a window ("Nd" or YYYY-MM-DD; empty
// is the server's default), as `icaro usage --json` prints it.
func (c *Client) Usage(ctx context.Context, since string) (*usage.Report, error) {
	var out usage.Report
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/usage?since="+url.QueryEscape(since), nil, &out)
	return &out, err
}
