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

// Confidence levels the model may report.
const (
	Low    = "low"
	Medium = "medium"
	High   = "high"
)

// Edit replaces one exact passage of a memory.
type Edit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

// Verdict is the model's decision about one memory. For needs_update the
// model returns Edits; Replacement is the memory with them applied.
type Verdict struct {
	Verdict     string `json:"verdict"`
	Confidence  string `json:"confidence"`
	Reason      string `json:"reason"`
	Edits       []Edit `json:"edits,omitempty"`
	Replacement string `json:"replacement,omitempty"`
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
- needs_update: a specific claim is now wrong, and the evidence shows what is true instead.
- obsolete: the memory as a whole no longer applies (the thing it describes was removed, retired or replaced) and should be forgotten.
- unsure: the evidence is not enough to decide.

Rules:
- A changed or deleted file is not by itself a contradiction. Only judge claims the evidence directly bears on, and check whether the memory already describes the change.
- Prefer unsure over guessing. Use confidence "high" only when a quoted claim is plainly contradicted by a quoted part of the evidence.
- For needs_update, "edits" lists the smallest corrections: each "find" is copied EXACTLY, character for character, from the memory (a whole sentence or clause, unique within it), and "replace" is the corrected text for that passage ("" to delete it). Do not rewrite anything else. Never add secrets.
- For every other verdict, "edits" is [].
- "reason" is at most three sentences and names the evidence (commit, file, or lookup) that decided it.`

var schema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict", "confidence", "reason", "edits"],
  "properties": {
    "verdict": {"type": "string", "enum": ["still_true", "needs_update", "obsolete", "unsure"]},
    "confidence": {"type": "string", "enum": ["low", "medium", "high"]},
    "reason": {"type": "string"},
    "edits": {
      "type": "array",
      "maxItems": 8,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["find", "replace"],
        "properties": {"find": {"type": "string"}, "replace": {"type": "string"}}
      }
    }
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
	v, err := parse(completion.Choices[0].Message.Content)
	if err != nil {
		return Verdict{}, err
	}
	if v.Verdict == NeedsUpdate {
		replacement, err := Apply(req.Content, v.Edits)
		if err != nil {
			// The model could not point at the claim it disputes; treat the
			// memory as undecided rather than propose a guessed rewrite.
			v.Verdict, v.Confidence, v.Edits = Unsure, Low, nil
			v.Reason = strings.TrimSpace(v.Reason + " (Proposed edits did not match the memory: " + err.Error() + ")")
			return v, nil
		}
		v.Replacement = replacement + fmt.Sprintf("\n\n(Corrected %s by memory-gardener review.)", req.Today.Format(time.DateOnly))
	}
	return v, nil
}

// Apply makes each edit to content. Every Find must occur exactly once.
func Apply(content string, edits []Edit) (string, error) {
	if len(edits) == 0 {
		return "", errors.New("no edits")
	}
	for i, e := range edits {
		if strings.TrimSpace(e.Find) == "" {
			return "", fmt.Errorf("edit %d has an empty find", i+1)
		}
		if e.Find == e.Replace {
			return "", fmt.Errorf("edit %d changes nothing", i+1)
		}
		switch n := strings.Count(content, e.Find); n {
		case 1:
			content = strings.Replace(content, e.Find, e.Replace, 1)
		case 0:
			return "", fmt.Errorf("edit %d: %q is not in the memory", i+1, truncate(e.Find, 60))
		default:
			return "", fmt.Errorf("edit %d: %q occurs %d times", i+1, truncate(e.Find, 60), n)
		}
	}
	return content, nil
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
	switch v.Confidence {
	case Low, Medium, High:
	default:
		v.Confidence = Low
	}
	switch v.Verdict {
	case StillTrue, Obsolete, Unsure:
		v.Edits = nil
	case NeedsUpdate:
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
