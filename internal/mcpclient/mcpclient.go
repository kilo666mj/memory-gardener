// Package mcpclient is a small Streamable HTTP MCP client for calling another
// service's tools with a bearer token and decoding their structured results.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configure a session.
type Options struct {
	Endpoint string
	Token    string
	Version  string
	// Headers are added to every request.
	Headers map[string]string
	// Interval is the minimum spacing between requests; zero means none.
	Interval time.Duration
}

// Session is a connected MCP client session.
type Session struct {
	name    string
	session *mcp.ClientSession
}

// ToolError is a tool call the server answered with an error result.
type ToolError struct {
	Tool    string
	Message string
}

func (e *ToolError) Error() string { return e.Tool + ": " + e.Message }

// Dial connects to an MCP endpoint. name prefixes error messages.
func Dial(ctx context.Context, name string, o Options) (*Session, error) {
	httpClient := &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &bearer{token: o.Token, headers: o.Headers, base: http.DefaultTransport, interval: o.Interval, maxWait: time.Minute},
	}
	transport := &mcp.StreamableClientTransport{Endpoint: o.Endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true}
	client := mcp.NewClient(&mcp.Implementation{Name: "memory-gardener", Version: o.Version}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", name, err)
	}
	return &Session{name: name, session: session}, nil
}

// Close ends the session.
func (s *Session) Close() error { return s.session.Close() }

// Call invokes tool with args and decodes its structured result into out.
// An error result from the tool comes back as a *ToolError.
func (s *Session) Call(ctx context.Context, tool string, args, out any) error {
	res, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return fmt.Errorf("%s %s: %w", s.name, tool, err)
	}
	if res.IsError {
		return fmt.Errorf("%s: %w", s.name, &ToolError{Tool: tool, Message: toolText(res)})
	}
	if out == nil {
		return nil
	}
	raw := []byte(toolText(res))
	if res.StructuredContent != nil {
		if raw, err = json.Marshal(res.StructuredContent); err != nil {
			return fmt.Errorf("%s %s: encode result: %w", s.name, tool, err)
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode result: %w", s.name, tool, err)
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

// bearer authenticates requests, adds fixed headers, spaces requests out so a
// batch job stays inside a per-client rate limit, and waits out a 429.
type bearer struct {
	token    string
	headers  map[string]string
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
		for k, v := range b.headers {
			req.Header.Set(k, v)
		}
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
