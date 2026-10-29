// Package rest is the transport of the icaro HTTP API: requests, bearer
// auth and error decoding, with no knowledge of the daemon's types. The
// desktop app talks to the daemon through it alone; package apiclient adds
// the typed calls the CLI and the stdio MCP server use.
package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"icaro/internal/workflow/issue"
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

// Error is an API error response.
type Error struct {
	Status int
	Msg    string
	Issues issue.Issues
	// Fields lists missing trigger fields (manual runs).
	Fields []string
}

func (e *Error) Error() string {
	if len(e.Issues) > 0 {
		return fmt.Sprintf("%s:\n  %s", e.Msg, strings.ReplaceAll(e.Issues.Error(), "; ", "\n  "))
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Msg, e.Status)
}

// DecodeError builds the Error for a failed response from its status and
// body.
func DecodeError(status int, data []byte) error {
	e := &Error{Status: status, Msg: http.StatusText(status)}
	var eb struct {
		Error  string       `json:"error"`
		Issues issue.Issues `json:"issues"`
		Fields []string     `json:"fields"`
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

// Do sends in as a JSON body (when not nil) and decodes the response into
// out; out may be *[]byte for a raw body.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return c.Send(ctx, method, path, "application/json", body, out)
}

// Send sends body with contentType (none when empty) and decodes the
// response into out as Do does.
func (c *Client) Send(ctx context.Context, method, path string, contentType string, body io.Reader, out any) error {
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
		return DecodeError(resp.StatusCode, data)
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

// Health checks the server.
func (c *Client) Health(ctx context.Context) error {
	return c.Send(ctx, http.MethodGet, "/healthz", "", nil, nil)
}
