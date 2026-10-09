// Package wayminder reads and revises memories through Wayminder's MCP tools.
package wayminder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/mcpclient"
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
	session *mcpclient.Session
}

// Dial connects to a Wayminder Streamable HTTP endpoint with a bearer token.
// Wayminder rate-limits per client (120 a minute by default), so requests are
// spaced out.
func Dial(ctx context.Context, endpoint, token, version string) (*Client, error) {
	session, err := mcpclient.Dial(ctx, "wayminder", mcpclient.Options{
		Endpoint: endpoint, Token: token, Version: version,
		Headers:  map[string]string{"X-Wayminder-Source": "memory-gardener"},
		Interval: 600 * time.Millisecond,
	})
	if err != nil {
		return nil, err
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
	err := c.session.Call(ctx, tool, args, out)
	var te *mcpclient.ToolError
	if errors.As(err, &te) && strings.Contains(strings.ToLower(te.Message), "not found") {
		return fmt.Errorf("%w: %w", ErrNotLive, err)
	}
	return err
}
