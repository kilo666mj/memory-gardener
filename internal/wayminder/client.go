// Package wayminder reads and revises memories through Wayminder's MCP tools.
package wayminder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Memory mirrors the fields of Wayminder's memory the gardener uses.
type Memory struct {
	ID           string    `json:"id"`
	Content      string    `json:"content"`
	Summary      string    `json:"summary,omitempty"`
	Kind         string    `json:"kind"`
	Scope        string    `json:"scope"`
	Tags         []string  `json:"tags,omitempty"`
	AuthorAgent  string    `json:"author_agent,omitempty"`
	SupersedesID string    `json:"supersedes_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Client is a connected Wayminder MCP session.
type Client struct {
	session *mcp.ClientSession
}

// Dial connects to a Wayminder Streamable HTTP endpoint with a bearer token.
func Dial(ctx context.Context, endpoint, token, version string) (*Client, error) {
	httpClient := &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &bearer{token: token, base: http.DefaultTransport, interval: 600 * time.Millisecond, maxWait: time.Minute},
	}
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true}
	client := mcp.NewClient(&mcp.Implementation{Name: "memory-gardener", Version: version}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to wayminder: %w", err)
	}
	return &Client{session: session}, nil
}

// Close ends the MCP session.
func (c *Client) Close() error { return c.session.Close() }

// ListAll enumerates every live memory. Wayminder lists one scope at a time
// (always mixed with global and personal), so the scope names come from
// status and results are de-duplicated by ID.
func (c *Client) ListAll(ctx context.Context) ([]Memory, error) {
	var status struct {
		Stats struct {
			ByScope map[string]int64 `json:"by_scope"`
		} `json:"stats"`
	}
	if err := c.call(ctx, "status", map[string]any{}, &status); err != nil {
		return nil, err
	}
	scopes := make([]string, 0, len(status.Stats.ByScope))
	for scope := range status.Stats.ByScope {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)

	seen := map[string]bool{}
	var all []Memory
	for _, scope := range scopes {
		before := ""
		for {
			args := map[string]any{"scope": scope, "limit": 100}
			if before != "" {
				args["before"] = before
			}
			var page struct {
				Memories []Memory `json:"memories"`
			}
			if err := c.call(ctx, "list_memories", args, &page); err != nil {
				return nil, fmt.Errorf("list scope %s: %w", scope, err)
			}
			for _, m := range page.Memories {
				if !seen[m.ID] {
					seen[m.ID] = true
					all = append(all, m)
				}
			}
			if len(page.Memories) < 100 {
				break
			}
			before = page.Memories[len(page.Memories)-1].ID
		}
	}
	return all, nil
}

// ErrNotLive reports that a memory was already superseded or forgotten.
var ErrNotLive = errors.New("memory is no longer live")

// Supersede replaces a memory's content, inheriting its other metadata.
func (c *Client) Supersede(ctx context.Context, id, content string) (Memory, error) {
	var out struct {
		Memory Memory `json:"memory"`
	}
	err := c.call(ctx, "supersede", map[string]any{"id": id, "content": content}, &out)
	return out.Memory, err
}

// Forget soft-deletes a memory.
func (c *Client) Forget(ctx context.Context, id string) error {
	var out json.RawMessage
	return c.call(ctx, "forget", map[string]any{"id": id}, &out)
}

func (c *Client) call(ctx context.Context, tool string, args map[string]any, out any) error {
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return fmt.Errorf("wayminder %s: %w", tool, err)
	}
	if res.IsError {
		msg := toolText(res)
		if strings.Contains(strings.ToLower(msg), "not found") {
			return fmt.Errorf("wayminder %s: %w: %s", tool, ErrNotLive, msg)
		}
		return fmt.Errorf("wayminder %s: %s", tool, msg)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return fmt.Errorf("wayminder %s: encode result: %w", tool, err)
	}
	if res.StructuredContent == nil {
		raw = []byte(toolText(res))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("wayminder %s: decode result: %w", tool, err)
	}
	return nil
}

func toolText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// bearer authenticates requests and stays inside Wayminder's per-client rate
// limit (120 a minute by default): it spaces requests out and waits out a 429.
type bearer struct {
	token    string
	base     http.RoundTripper
	interval time.Duration
	maxWait  time.Duration

	mu   sync.Mutex
	next time.Time
}

func (b *bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := b.pace(r.Context()); err != nil {
			return nil, err
		}
		req := r.Clone(r.Context())
		if r.Body != nil && r.GetBody != nil {
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
		req.Header.Set("Authorization", "Bearer "+b.token)
		req.Header.Set("X-Wayminder-Source", "memory-gardener")
		resp, err := b.base.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || attempt == 3 || (r.Body != nil && r.GetBody == nil) {
			return resp, err
		}
		wait := b.maxWait
		if secs, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && time.Duration(secs)*time.Second < wait {
			wait = time.Duration(secs) * time.Second
		}
		_ = resp.Body.Close()
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(wait):
		}
	}
}

func (b *bearer) pace(ctx context.Context) error {
	b.mu.Lock()
	now := time.Now()
	at := b.next
	if at.Before(now) {
		at = now
	}
	b.next = at.Add(b.interval)
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(at)):
		return nil
	}
}
