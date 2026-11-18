package distill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Completer runs one schema-constrained completion.
type Completer interface {
	Complete(ctx context.Context, system, user string, schema json.RawMessage, maxTokens int) (string, error)
}

// Client talks to an OpenAI-compatible endpoint: llama-server (managed,
// over a Unix socket) or one the user runs (llama-server, Ollama).
type Client struct {
	// BaseURL is the endpoint, e.g. http://127.0.0.1:11434 (no /v1).
	BaseURL string
	// Model names the model for endpoints that serve several (Ollama);
	// llama-server ignores it.
	Model string
	HTTP  *http.Client
}

// UnixClient talks to a llama-server listening on a Unix socket.
func UnixClient(socket string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{BaseURL: "http://distill", HTTP: &http.Client{Transport: tr}}
}

// maxResponse caps a completion response.
const maxResponse = 1 << 20

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) url(path string) string { return strings.TrimRight(c.BaseURL, "/") + path }

// Ready reports whether the endpoint answers: llama-server's /health, or
// the OpenAI-compatible /v1/models for other servers.
func (c *Client) Ready(ctx context.Context) error {
	var last error
	for _, p := range []string{"/health", "/v1/models"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(p), nil)
		if err != nil {
			return err
		}
		resp, err := c.http().Do(req)
		if err != nil {
			last = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		last = fmt.Errorf("%s: %s", p, resp.Status)
	}
	return last
}

// WaitReady polls Ready until it succeeds or ctx ends.
func (c *Client) WaitReady(ctx context.Context, every time.Duration) error {
	for {
		err := c.Ready(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the model server did not become ready: %w", err)
		case <-time.After(every):
		}
	}
}

type chatRequest struct {
	Model          string         `json:"model,omitempty"`
	Messages       []message      `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
	Temperature    float64        `json:"temperature"`
	MaxTokens      int            `json:"max_tokens"`
	// Thinking models (Qwen3.5) spend the whole budget reasoning unless
	// it is turned off; llama-server reads this, others ignore it.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	Think              *bool          `json:"think,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// ErrNoContent reports a completion with no answer (a model that only
// reasoned, or stopped at once).
var ErrNoContent = errors.New("the model returned no answer")

// Complete asks for one completion whose output is constrained to schema.
func (c *Client) Complete(ctx context.Context, system, user string, schema json.RawMessage, maxTokens int) (string, error) {
	off := false
	body, err := json.Marshal(chatRequest{
		Model:    c.Model,
		Messages: []message{{Role: "system", Content: system}, {Role: "user", Content: user}},
		ResponseFormat: responseFormat{Type: "json_schema", JSONSchema: jsonSchema{
			Name: "memory", Strict: true, Schema: schema,
		}},
		Temperature: 0, MaxTokens: maxTokens,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false}, Think: &off,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the model server answered %s: %s", resp.Status, clip(string(raw), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("the model server's answer is not JSON: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", ErrNoContent
	}
	return out.Choices[0].Message.Content, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
