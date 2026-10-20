package apiclient

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"icaro/internal/memory"
)

// MemoryQuery are the filters of GET /api/v1/memory.
type MemoryQuery struct {
	Project      string
	Text         string
	Kinds        []string
	Branch       string
	Agent        string
	SourcePrefix string
	Status       string
	Since        *time.Time
	Limit        int
}

// WriteMemory stores a memory.
func (c *Client) WriteMemory(ctx context.Context, req memory.RESTWrite) (*memory.Memory, error) {
	var out memory.Memory
	err := c.doJSON(ctx, "POST", "/api/v1/memory", req, &out)
	return &out, err
}

// QueryMemory searches a project's memory.
func (c *Client) QueryMemory(ctx context.Context, q MemoryQuery) (*memory.QueryResponse, error) {
	v := url.Values{"project": {q.Project}}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("q", q.Text)
	set("kind", strings.Join(q.Kinds, ","))
	set("branch", q.Branch)
	set("agent", q.Agent)
	set("source", q.SourcePrefix)
	set("status", q.Status)
	if q.Since != nil {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var out memory.QueryResponse
	err := c.doJSON(ctx, "GET", "/api/v1/memory?"+v.Encode(), nil, &out)
	return &out, err
}

// MemoryProvenance returns one memory with provenance.
func (c *Client) MemoryProvenance(ctx context.Context, project, id string) (*memory.Provenance, error) {
	var out memory.Provenance
	err := c.doJSON(ctx, "GET", "/api/v1/memory/"+url.PathEscape(id)+"?project="+url.QueryEscape(project), nil, &out)
	return &out, err
}

// PendingHandoffs lists pending handoffs.
func (c *Client) PendingHandoffs(ctx context.Context, project, branch string) (*memory.QueryResponse, error) {
	v := url.Values{"project": {project}}
	if branch != "" {
		v.Set("branch", branch)
	}
	var out memory.QueryResponse
	err := c.doJSON(ctx, "GET", "/api/v1/memory/handoffs?"+v.Encode(), nil, &out)
	return &out, err
}

// AcceptHandoff accepts a pending handoff.
func (c *Client) AcceptHandoff(ctx context.Context, req memory.RESTAccept, id string) (*memory.Memory, error) {
	var out memory.Memory
	err := c.doJSON(ctx, "POST", "/api/v1/memory/"+url.PathEscape(id)+"/accept", req, &out)
	return &out, err
}

// IngestResult is the reply of POST /api/v1/memory/ingest.
type IngestResult struct {
	Project string               `json:"project"`
	Report  *memory.IngestReport `json:"report"`
}

// IngestMemory asks the daemon to index a directory.
func (c *Client) IngestMemory(ctx context.Context, req memory.RESTIngest) (*IngestResult, error) {
	var out IngestResult
	err := c.doJSON(ctx, "POST", "/api/v1/memory/ingest", req, &out)
	return &out, err
}

// ImportMemory loads another tool's export.
func (c *Client) ImportMemory(ctx context.Context, req memory.RESTImport) (*memory.ImportReport, error) {
	var out memory.ImportReport
	err := c.doJSON(ctx, "POST", "/api/v1/memory/import", req, &out)
	return &out, err
}
