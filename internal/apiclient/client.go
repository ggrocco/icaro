// Package apiclient is the Go client for the icaro HTTP API, used by the
// CLI and by the stdio MCP server.
package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"icaro/internal/service"
	"icaro/internal/workflow"
)

// Client talks to an icaro server.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New creates a client.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// NewWithHTTP creates a client over a custom transport (e.g. the daemon's
// Unix socket).
func NewWithHTTP(baseURL, token string, hc *http.Client) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: hc}
}

// Do performs a raw request against the API; out may be *[]byte for raw
// bodies. Exposed for commands added by other packages.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	return c.doJSON(ctx, method, path, in, out)
}

// Error is an API error response.
type Error struct {
	Status int
	Msg    string
	Issues workflow.Issues
	// Fields lists missing trigger fields (manual runs).
	Fields []string
}

func (e *Error) Error() string {
	if len(e.Issues) > 0 {
		return fmt.Sprintf("%s:\n  %s", e.Msg, strings.ReplaceAll(e.Issues.Error(), "; ", "\n  "))
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Msg, e.Status)
}

func decodeError(status int, data []byte) error {
	e := &Error{Status: status, Msg: http.StatusText(status)}
	var eb struct {
		Error  string          `json:"error"`
		Issues workflow.Issues `json:"issues"`
		Fields []string        `json:"fields"`
	}
	if json.Unmarshal(data, &eb) == nil && eb.Error != "" {
		e.Msg, e.Issues, e.Fields = eb.Error, eb.Issues, eb.Fields
	}
	return e
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return decodeError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return c.do(ctx, method, path, "application/json", body, out)
}

// Health checks the server.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", "", nil, nil)
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
		return decodeError(resp.StatusCode, data)
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
