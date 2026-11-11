package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"icaro/internal/apiclient/rest"
	"icaro/internal/assist"
)

func chatPath(id string, rest ...string) string {
	p := "/api/v1/chats/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p
}

// ListChats lists the assistant's chats.
func (c *Client) ListChats(ctx context.Context) ([]assist.ChatInfo, error) {
	var out struct {
		Chats []assist.ChatInfo `json:"chats"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/chats", nil, &out)
	return out.Chats, err
}

// CreateChat starts a chat.
func (c *Client) CreateChat(ctx context.Context, req assist.CreateRequest) (*assist.ChatDetail, error) {
	var out assist.ChatDetail
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/chats", req, &out)
	return &out, err
}

// GetChat returns a chat with its messages, draft and pending approvals.
func (c *Client) GetChat(ctx context.Context, id string) (*assist.ChatDetail, error) {
	var out assist.ChatDetail
	err := c.doJSON(ctx, http.MethodGet, chatPath(id), nil, &out)
	return &out, err
}

// SendChat sends a message; it returns the message and the queued reply.
func (c *Client) SendChat(ctx context.Context, id, text string) ([]assist.MessageInfo, error) {
	var out struct {
		Messages []assist.MessageInfo `json:"messages"`
	}
	err := c.doJSON(ctx, http.MethodPost, chatPath(id, "messages"), map[string]string{"text": text}, &out)
	return out.Messages, err
}

// CancelChat stops the chat's running turn and drops its queued ones.
func (c *Client) CancelChat(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, chatPath(id, "cancel"), nil, nil)
}

// DecideChat approves or declines a gated call.
func (c *Client) DecideChat(ctx context.Context, id, approval string, approve bool) (*assist.ApprovalInfo, error) {
	verb := "decline"
	if approve {
		verb = "approve"
	}
	var out assist.ApprovalInfo
	err := c.doJSON(ctx, http.MethodPost, chatPath(id, "approvals", approval, verb), nil, &out)
	return &out, err
}

// DeleteChat removes a chat.
func (c *Client) DeleteChat(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, chatPath(id), nil, nil)
}

// ChatEntry is one entry of a chat's event stream and the offset after it.
type ChatEntry struct {
	Offset int64
	Data   json.RawMessage
	// Resync is set when the server asks the client to reconnect.
	Resync bool
}

// ChatEvents follows a chat's log from offset, calling fn per entry until
// fn returns false, the server asks for a resync, or ctx ends.
func (c *Client) ChatEvents(ctx context.Context, id string, offset int64, fn func(ChatEntry) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+chatPath(id, "events")+"?offset="+strconv.FormatInt(offset, 10), nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "text/event-stream")
	hc := *c.HTTP
	hc.Timeout = 0
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
	var name string
	var e ChatEntry
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if name == "entry" || name == "resync" {
				e.Resync = name == "resync"
				if !fn(e) || e.Resync {
					return nil
				}
			}
			name, e = "", ChatEntry{}
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			e.Offset, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "data: "):
			e.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}
