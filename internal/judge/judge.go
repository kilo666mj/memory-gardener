// Package judge asks an OpenAI-compatible model whether a memory still holds.
package judge

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
)

// Verdicts the model may return.
const (
	StillTrue   = "still_true"
	NeedsUpdate = "needs_update"
	Obsolete    = "obsolete"
	Unsure      = "unsure"
)

// Verdict is the model's decision about one memory.
type Verdict struct {
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason"`
	Replacement string `json:"replacement"`
}

// Request is one memory and the evidence against it.
type Request struct {
	MemoryID string
	Scope    string
	Kind     string
	Content  string
	Anchor   time.Time
	Signals  []string
	Evidence []string
	Today    time.Time
}

// Client talks to the chat completions endpoint.
type Client struct {
	BaseURL string
	Model   string
	APIKey  string
	HTTP    *http.Client
}

// ErrUnavailable reports that the model server could not be reached, so the
// memory should be judged on a later run.
var ErrUnavailable = errors.New("judge unavailable")

const system = `You maintain a store of durable engineering memories written by coding agents.
Decide whether one memory is still accurate, using ONLY the evidence given.

Verdicts:
- still_true: the evidence does not contradict any claim in the memory.
- needs_update: some claims are now wrong or out of date, but the memory is still useful once corrected.
- obsolete: the memory as a whole no longer applies (the thing it describes was removed, retired or replaced) and should be forgotten.
- unsure: the evidence is not enough to decide.

Rules:
- A changed file is not by itself a contradiction; only judge claims the evidence actually bears on.
- Prefer unsure over guessing.
- For needs_update, "replacement" is the complete corrected memory: keep every claim the evidence does not contradict, keep the original style and level of detail, fix only what the evidence shows, and replace the verification date with "Verified <today> by memory-gardener against <short evidence reference>". Never add secrets.
- For every other verdict, "replacement" is "".
- "reason" is at most three sentences and names the evidence (commit, file, or lookup) that decided it.`

var schema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict", "reason", "replacement"],
  "properties": {
    "verdict": {"type": "string", "enum": ["still_true", "needs_update", "obsolete", "unsure"]},
    "reason": {"type": "string"},
    "replacement": {"type": "string"}
  }
}`)

// Judge asks the model for a verdict.
func (c *Client) Judge(ctx context.Context, req Request) (Verdict, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Today: %s\nMemory %s (scope %s, kind %s), last known true %s:\n<memory>\n%s\n</memory>\n\n",
		req.Today.Format(time.DateOnly), req.MemoryID, req.Scope, req.Kind, req.Anchor.Format(time.DateOnly), req.Content)
	user.WriteString("Mechanical findings:\n")
	for _, s := range req.Signals {
		user.WriteString("- " + s + "\n")
	}
	user.WriteString("\nEvidence:\n")
	for _, e := range req.Evidence {
		user.WriteString("<evidence>\n" + e + "\n</evidence>\n")
	}

	body, err := json.Marshal(map[string]any{
		"model":       c.Model,
		"temperature": 0,
		"max_tokens":  4096,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user.String()},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "memory_verdict", "strict": true, "schema": schema},
		},
	})
	if err != nil {
		return Verdict{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusServiceUnavailable {
		return Verdict{}, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("judge: status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil || len(completion.Choices) == 0 {
		return Verdict{}, fmt.Errorf("judge: unexpected response: %s", truncate(string(raw), 300))
	}
	return parse(completion.Choices[0].Message.Content)
}

func parse(content string) (Verdict, error) {
	content = strings.TrimSpace(content)
	// Tolerate a reasoning preamble or code fence around the JSON object.
	if i := strings.Index(content, "{"); i > 0 {
		content = content[i:]
	}
	if j := strings.LastIndex(content, "}"); j >= 0 {
		content = content[:j+1]
	}
	var v Verdict
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		return Verdict{}, fmt.Errorf("judge: verdict is not JSON: %w", err)
	}
	switch v.Verdict {
	case StillTrue, Obsolete, Unsure:
		v.Replacement = ""
	case NeedsUpdate:
		if strings.TrimSpace(v.Replacement) == "" {
			return Verdict{}, errors.New("judge: needs_update without a replacement")
		}
	default:
		return Verdict{}, fmt.Errorf("judge: unknown verdict %q", v.Verdict)
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
