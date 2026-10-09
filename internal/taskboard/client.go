// Package taskboard calls the Taskboard MCP tools the gardener needs: start a
// review task, ask its question, read the answer, and close the task.
// Taskboard accepts agent bearer tokens only on its MCP endpoint; the REST API
// is for browser sessions.
package taskboard

import (
	"context"
	"errors"
	"strings"

	"github.com/kilo666mj/memory-gardener/internal/mcpclient"
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

// ErrNotFound reports a task that does not exist or is not visible.
var ErrNotFound = errors.New("task not found")

// Caller calls one MCP tool; *mcpclient.Session implements it.
type Caller interface {
	Call(ctx context.Context, tool string, args, out any) error
}

// Client calls Taskboard's MCP tools.
type Client struct {
	MCP        Caller
	SessionKey string
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
	err := c.call(ctx, "task_start", req, &out)
	return out.Task, out.Run, err
}

// Note adds a note message from an active run.
func (c *Client) Note(ctx context.Context, taskID, runID, body, key string) error {
	return c.call(ctx, "task_message_add", map[string]any{
		"task_id": taskID, "author_run_id": runID, "kind": "note", "body": body, "idempotency_key": key,
	}, nil)
}

// Escalate asks a question. A blocking question ends the run and leaves the
// task waiting until a person answers.
func (c *Client) Escalate(ctx context.Context, taskID string, req EscalationRequest) (Escalation, error) {
	var out struct {
		Escalation Escalation `json:"escalation"`
	}
	err := c.call(ctx, "task_escalate", struct {
		TaskID string `json:"task_id"`
		EscalationRequest
	}{taskID, req}, &out)
	return out.Escalation, err
}

// Get reads a task.
func (c *Client) Get(ctx context.Context, taskID string) (Task, error) {
	var out struct {
		Task Task `json:"task"`
	}
	err := c.call(ctx, "task_get", map[string]any{"task_id": taskID}, &out)
	return out.Task, err
}

// Escalations lists a task's questions.
func (c *Client) Escalations(ctx context.Context, taskID string) ([]Escalation, error) {
	var out struct {
		Escalations []Escalation `json:"escalations"`
	}
	err := c.call(ctx, "task_escalation_list", map[string]any{"task_id": taskID}, &out)
	return out.Escalations, err
}

// Messages lists a task's messages.
func (c *Client) Messages(ctx context.Context, taskID string) ([]Message, error) {
	var out struct {
		Messages []Message `json:"messages"`
	}
	err := c.call(ctx, "task_message_list", map[string]any{"task_id": taskID, "limit": 200}, &out)
	return out.Messages, err
}

// Claim takes a queued task and returns the new run.
func (c *Client) Claim(ctx context.Context, taskID string, version int64) (Task, Run, error) {
	var out struct {
		Task Task `json:"task"`
		Run  Run  `json:"run"`
	}
	err := c.call(ctx, "task_claim", map[string]any{
		"task_id": taskID, "expected_version": version,
		"agent": agentName, "client": agentName, "agent_session_key": c.SessionKey,
	}, &out)
	return out.Task, out.Run, err
}

// Complete marks every open item done and finishes the task with a note.
func (c *Client) Complete(ctx context.Context, task Task, runID, note string) error {
	open := []string{}
	for _, item := range task.Items {
		if item.Status != "done" && item.Status != "skipped" {
			open = append(open, item.ID)
		}
	}
	return c.call(ctx, "task_complete", map[string]any{
		"task_id": task.ID, "expected_version": task.Version, "run_id": runID,
		"complete_item_ids": open, "current_note": note,
	}, nil)
}

func (c *Client) call(ctx context.Context, tool string, args, out any) error {
	err := c.MCP.Call(ctx, tool, args, out)
	var te *mcpclient.ToolError
	if errors.As(err, &te) && strings.Contains(strings.ToLower(te.Message), "not found") {
		return errors.Join(ErrNotFound, err)
	}
	return err
}
