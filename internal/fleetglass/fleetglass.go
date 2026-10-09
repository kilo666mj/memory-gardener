// Package fleetglass posts checks to Fleetglass's ingest API.
package fleetglass

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Check is one Fleetglass check record.
type Check struct {
	Source     string         `json:"source"`
	Host       string         `json:"host"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Summary    string         `json:"summary"`
	ObservedAt string         `json:"observed_at"`
	Data       map[string]any `json:"data,omitempty"`
}

// Client posts checks. A zero BaseURL makes Post a no-op.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Post sends checks in one request.
func (c *Client) Post(ctx context.Context, checks []Check) error {
	if c.BaseURL == "" || len(checks) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range checks {
		if checks[i].ObservedAt == "" {
			checks[i].ObservedAt = now
		}
	}
	raw, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/ingest/checks", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fleetglass: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("fleetglass: status %d: %s", resp.StatusCode, body)
	}
	return nil
}
