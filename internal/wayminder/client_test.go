package wayminder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type listArgs struct {
	Scope  string `json:"scope,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Before string `json:"before,omitempty"`
}

type idArgs struct {
	ID      string `json:"id"`
	Content string `json:"content,omitempty"`
}

// fakeServer mimics Wayminder: list_memories returns the requested scope
// mixed with global and personal, newest first, in pages.
func fakeServer(t *testing.T, store map[string][]Memory, token string) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-wayminder"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "status"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]any, error) {
		byScope := map[string]int{}
		for scope, ms := range store {
			byScope[scope] = len(ms)
		}
		return nil, map[string]any{"stats": map[string]any{"by_scope": byScope}}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "list_memories"}, func(_ context.Context, _ *mcp.CallToolRequest, in listArgs) (*mcp.CallToolResult, map[string]any, error) {
		var all []Memory
		for scope := range map[string]bool{"global": true, "personal": true, in.Scope: true} {
			all = append(all, store[scope]...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
		var page []Memory
		for _, m := range all {
			if in.Before != "" && m.ID >= in.Before {
				continue
			}
			if len(page) == in.Limit {
				break
			}
			page = append(page, m)
		}
		return nil, map[string]any{"memories": page}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "supersede"}, func(_ context.Context, _ *mcp.CallToolRequest, in idArgs) (*mcp.CallToolResult, map[string]any, error) {
		if in.ID == "gone" {
			return nil, nil, fmt.Errorf("live memory %s not found", in.ID)
		}
		return nil, map[string]any{"memory": Memory{ID: "new-" + in.ID, Content: in.Content}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestListAllPagesEveryScopeOnce(t *testing.T) {
	store := map[string][]Memory{"global": {{ID: "G1", Scope: "global"}}, "personal": {{ID: "P1", Scope: "personal"}}}
	for i := range 150 {
		store["repo:x"] = append(store["repo:x"], Memory{ID: fmt.Sprintf("X%03d", i), Scope: "repo:x"})
	}
	store["host:h"] = []Memory{{ID: "H1", Scope: "host:h"}}
	srv := fakeServer(t, store, "secret")

	c, err := Dial(context.Background(), srv.URL, "secret", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	all, err := c.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 153 {
		t.Fatalf("got %d memories, want 153", len(all))
	}
	seen := map[string]bool{}
	for _, m := range all {
		if seen[m.ID] {
			t.Fatalf("duplicate %s", m.ID)
		}
		seen[m.ID] = true
	}
}

func TestSupersedeMapsNotFound(t *testing.T) {
	srv := fakeServer(t, map[string][]Memory{}, "secret")
	c, err := Dial(context.Background(), srv.URL, "secret", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	m, err := c.Supersede(context.Background(), "A", "new text")
	if err != nil || m.ID != "new-A" {
		t.Fatalf("supersede = %+v, %v", m, err)
	}
	if _, err := c.Supersede(context.Background(), "gone", "x"); !errors.Is(err, ErrNotLive) {
		t.Fatalf("err = %v, want ErrNotLive", err)
	}
}

func TestDialRejectsBadToken(t *testing.T) {
	srv := fakeServer(t, map[string][]Memory{}, "secret")
	if _, err := Dial(context.Background(), srv.URL, "wrong", "test"); err == nil {
		t.Fatal("connected with a bad token")
	}
}

func TestBearerWaitsOutRateLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("attempt %d: body %q auth %q", calls, body, r.Header.Get("Authorization"))
		}
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := &http.Client{Transport: &bearer{token: "tok", base: http.DefaultTransport, interval: time.Millisecond, maxWait: time.Second}}
	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 2 {
		t.Fatalf("status %d after %d calls", resp.StatusCode, calls)
	}
}
