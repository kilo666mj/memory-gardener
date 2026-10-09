// Package taskboard is a small REST client for the Taskboard calls the
// gardener makes: start a review task, ask its question, read the answer,
// and close the task.
package taskboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Task is the subset of a Taskboard task the gardener reads.
type Task struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version int64  `json:"version"`
	Items   []Item `json:"items"`
}

// Item is a checklist item.
type Item struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

// Run is an agent run.
type Run struct {
	ID string `json:"id"`
}

// Escalation is a structured question and its answer.
type Escalation struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	SelectedOption  string `json:"selected_option"`
	AnswerMessageID string `json:"answer_message_id"`
	ResolvedBy      string `json:"resolved_by"`
}

// Message is a task message.
type Message struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Body string `json:"body"`
}

// StartRequest creates a task with an active run.
type StartRequest struct {
	Title           string   `json:"title"`
	Type            string   `json:"type,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	Section         string   `json:"section,omitempty"`
	Project         string   `json:"project,omitempty"`
	Repository      string   `json:"repository,omitempty"`
	Priority        string   `json:"priority,omitempty"`
	Checklist       []string `json:"checklist"`
	Agent           string   `json:"agent,omitempty"`
	Client          string   `json:"client,omitempty"`
	AgentSessionKey string   `json:"agent_session_key,omitempty"`
	ForceNew        bool     `json:"force_new,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty"`
}

// EscalationRequest asks a question from an active run.
type EscalationRequest struct {
	RunID           string   `json:"run_id"`
	ExpectedVersion int64    `json:"expected_version"`
	Question        string   `json:"question"`
	Options         []string `json:"options,omitempty"`
	Recommendation  string   `json:"recommendation,omitempty"`
	Blocking        bool     `json:"blocking"`
	Answerers       []string `json:"answerers,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty"`
}

// Client calls the Taskboard REST API.
type Client struct {
	BaseURL    string
	Token      string
	SessionKey string
	HTTP       *http.Client
}

const agentName = "memory-gardener"

// Start creates a task with an active run.
func (c *Client) Start(ctx context.Context, req StartRequest) (Task, Run, error) {
	req.Agent, req.Client, req.AgentSessionKey = agentName, agentName, c.SessionKey
	// Each memory gets its own review task even when titles look alike.
	req.ForceNew = true
	var out struct {
		Task Task `json:"task"`
		Run  Run  `json:"run"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/tasks", req, &out)
	return out.Task, out.Run, err
}

// Note adds a note message from an active run.
func (c *Client) Note(ctx context.Context, taskID, runID, body, key string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/messages", map[string]any{
		"author_run_id": runID, "kind": "note", "body": body, "idempotency_key": key,
	}, nil)
}

// Escalate asks a question. A blocking question ends the run and leaves the
// task waiting until a person answers.
func (c *Client) Escalate(ctx context.Context, taskID string, req EscalationRequest) (Escalation, error) {
	var out struct {
		Escalation Escalation `json:"escalation"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/escalations", req, &out)
	return out.Escalation, err
}

// Get reads a task.
func (c *Client) Get(ctx context.Context, taskID string) (Task, error) {
	var out struct {
		Task Task `json:"task"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, &out)
	return out.Task, err
}

// Escalations lists a task's questions.
func (c *Client) Escalations(ctx context.Context, taskID string) ([]Escalation, error) {
	var out struct {
		Escalations []Escalation `json:"escalations"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/escalations", nil, &out)
	return out.Escalations, err
}

// Messages lists a task's messages.
func (c *Client) Messages(ctx context.Context, taskID string) ([]Message, error) {
	var out struct {
		Messages []Message `json:"messages"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/messages", nil, &out)
	return out.Messages, err
}

// Claim takes a queued task and returns the new run.
func (c *Client) Claim(ctx context.Context, taskID string, version int64) (Task, Run, error) {
	var out struct {
		Task Task `json:"task"`
		Run  Run  `json:"run"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/runs", map[string]any{
		"expected_version": version, "agent": agentName, "client": agentName, "agent_session_key": c.SessionKey,
	}, &out)
	return out.Task, out.Run, err
}

// Complete marks every open item done and finishes the task with a note.
func (c *Client) Complete(ctx context.Context, task Task, runID, note string) error {
	var open []string
	for _, item := range task.Items {
		if item.Status != "done" && item.Status != "skipped" {
			open = append(open, item.ID)
		}
	}
	return c.do(ctx, http.MethodPatch, "/api/v1/tasks/"+url.PathEscape(task.ID), map[string]any{
		"expected_version": task.Version, "run_id": runID, "status": "done",
		"complete_item_ids": open, "current_note": note,
	}, nil)
}

// StatusError is a non-2xx response.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("taskboard: status %d: %s", e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("taskboard %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("taskboard %s %s: decode: %w", method, path, err)
	}
	return nil
}
