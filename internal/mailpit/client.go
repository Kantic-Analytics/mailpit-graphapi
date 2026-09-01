package mailpit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Address struct {
	Name    string `json:"Name"`
	Address string `json:"Address"`
}

type MessageSummary struct {
	ID          string    `json:"ID"`
	MessageID   string    `json:"MessageID"`
	Read        bool      `json:"Read"`
	From        Address   `json:"From"`
	To          []Address `json:"To"`
	Cc          []Address `json:"Cc"`
	Bcc         []Address `json:"Bcc"`
	ReplyTo     []Address `json:"ReplyTo"`
	Subject     string    `json:"Subject"`
	Created     time.Time `json:"Created"`
	Tags        []string  `json:"Tags"`
	Snippet     string    `json:"Snippet"`
	Attachments int       `json:"Attachments"`
	Size        uint64    `json:"Size"`
}

type Message struct {
	ID          string       `json:"ID"`
	MessageID   string       `json:"MessageID"`
	Date        time.Time    `json:"Date"`
	From        Address      `json:"From"`
	To          []Address    `json:"To"`
	Cc          []Address    `json:"Cc"`
	Bcc         []Address    `json:"Bcc"`
	ReplyTo     []Address    `json:"ReplyTo"`
	Subject     string       `json:"Subject"`
	Text        string       `json:"Text"`
	HTML        string       `json:"HTML"`
	Tags        []string     `json:"Tags"`
	Attachments []Attachment `json:"Attachments"`
	Size        uint64       `json:"Size"`
}

type Attachment struct {
	PartID      string `json:"PartID"`
	FileName    string `json:"FileName"`
	ContentType string `json:"ContentType"`
	Size        uint64 `json:"Size"`
}

type ListResponse struct {
	Total          int              `json:"total"`
	Unread         int              `json:"unread"`
	MessagesCount  int              `json:"messages_count"`
	MessagesUnread int              `json:"messages_unread"`
	Start          int              `json:"start"`
	Messages       []MessageSummary `json:"messages"`
}

type SendAddress struct {
	Name  string `json:"Name,omitempty"`
	Email string `json:"Email"`
}

type SendRequest struct {
	From    SendAddress       `json:"From"`
	To      []SendAddress     `json:"To,omitempty"`
	Cc      []SendAddress     `json:"Cc,omitempty"`
	Bcc     []string          `json:"Bcc,omitempty"`
	ReplyTo []SendAddress     `json:"ReplyTo,omitempty"`
	Subject string            `json:"Subject,omitempty"`
	Text    string            `json:"Text,omitempty"`
	HTML    string            `json:"HTML,omitempty"`
	Headers map[string]string `json:"Headers,omitempty"`
	Tags    []string          `json:"Tags,omitempty"`
}

type Client struct {
	base *url.URL
	http *http.Client
}

func New(rawURL string, client *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid Mailpit URL %q", rawURL)
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{base: u, http: client}, nil
}

func (c *Client) List(ctx context.Context, start, limit int) (ListResponse, error) {
	var out ListResponse
	q := url.Values{"start": {strconv.Itoa(start)}, "limit": {strconv.Itoa(limit)}}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/messages?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) Get(ctx context.Context, id string) (Message, error) {
	var out Message
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/message/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) Raw(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/message/"+url.PathEscape(id)+"/raw", nil, "")
}

func (c *Client) Headers(ctx context.Context, id string) (map[string][]string, error) {
	var out map[string][]string
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/message/"+url.PathEscape(id)+"/headers", nil, &out)
	return out, err
}

func (c *Client) SetRead(ctx context.Context, id string, read bool) error {
	_, err := c.do(ctx, http.MethodPut, "/api/v1/messages", map[string]any{"IDs": []string{id}, "Read": read}, "application/json")
	return err
}

func (c *Client) SetTags(ctx context.Context, id string, tags []string) error {
	_, err := c.do(ctx, http.MethodPut, "/api/v1/tags", map[string]any{"IDs": []string{id}, "Tags": tags}, "application/json")
	return err
}

func (c *Client) Send(ctx context.Context, message SendRequest) (string, error) {
	var out struct {
		ID string `json:"ID"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/send", message, &out)
	return out.ID, err
}

func (c *Client) Ready(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/api/v1/info", nil, "")
	return err
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	b, err := c.do(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode Mailpit response: %w", err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, contentType string) ([]byte, error) {
	var r io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Mailpit request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Mailpit returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}
