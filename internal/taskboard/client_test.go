package taskboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/kilo666mj/memory-gardener/internal/mcpclient"
)

// recorder is a Caller that records calls and replies with canned JSON.
type recorder struct {
	calls   []string
	args    map[string]map[string]any
	replies map[string]string
	fail    map[string]error
}

func (r *recorder) Call(_ context.Context, tool string, args, out any) error {
	r.calls = append(r.calls, tool)
	raw, _ := json.Marshal(args)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	r.args[tool] = m
	if err := r.fail[tool]; err != nil {
		return err
	}
	if out != nil && r.replies[tool] != "" {
		return json.Unmarshal([]byte(r.replies[tool]), out)
	}
	return nil
}

func TestClientSendsTaskboardToolArguments(t *testing.T) {
	r := &recorder{args: map[string]map[string]any{}, replies: map[string]string{
		"task_start":    `{"task":{"id":"T1","status":"active","version":1,"items":[{"id":"I1","status":"todo"},{"id":"I2","status":"done"}]},"run":{"id":"R1"}}`,
		"task_escalate": `{"escalation":{"id":"E1","status":"open"}}`,
		"task_get":      `{"task":{"id":"T1","status":"queued","version":4}}`,
		"task_claim":    `{"task":{"id":"T1","version":5},"run":{"id":"R2"}}`,
	}}
	c := &Client{MCP: r, SessionKey: "k"}
	ctx := context.Background()

	task, run, err := c.Start(ctx, StartRequest{Title: "x", Checklist: []string{"a"}})
	if err != nil || task.ID != "T1" || run.ID != "R1" {
		t.Fatalf("start = %+v %+v %v", task, run, err)
	}
	if a := r.args["task_start"]; a["force_new"] != true || a["agent"] != "memory-gardener" || a["agent_session_key"] != "k" {
		t.Errorf("task_start args = %v", a)
	}
	if err := c.Note(ctx, "T1", "R1", "body", "key"); err != nil {
		t.Fatal(err)
	}
	if a := r.args["task_message_add"]; a["task_id"] != "T1" || a["author_run_id"] != "R1" || a["kind"] != "note" {
		t.Errorf("task_message_add args = %v", a)
	}
	esc, err := c.Escalate(ctx, "T1", EscalationRequest{RunID: "R1", ExpectedVersion: 3, Question: "q", Options: []string{"a", "b"}, Blocking: true})
	if err != nil || esc.ID != "E1" {
		t.Fatalf("escalate = %+v %v", esc, err)
	}
	if a := r.args["task_escalate"]; a["task_id"] != "T1" || a["run_id"] != "R1" || a["blocking"] != true || a["expected_version"] != float64(3) {
		t.Errorf("task_escalate args = %v", a)
	}
	got, err := c.Get(ctx, "T1")
	if err != nil || got.Version != 4 {
		t.Fatalf("get = %+v %v", got, err)
	}
	claimed, run2, err := c.Claim(ctx, "T1", 4)
	if err != nil || run2.ID != "R2" || r.args["task_claim"]["expected_version"] != float64(4) {
		t.Fatalf("claim = %+v %+v %v %v", claimed, run2, err, r.args["task_claim"])
	}
	if err := c.Complete(ctx, task, "R2", "done"); err != nil {
		t.Fatal(err)
	}
	if a := r.args["task_complete"]; fmt.Sprint(a["complete_item_ids"]) != "[I1]" || a["run_id"] != "R2" {
		t.Errorf("task_complete args = %v", a)
	}
}

func TestNotFoundMapsToSentinel(t *testing.T) {
	r := &recorder{args: map[string]map[string]any{}, fail: map[string]error{
		"task_get": fmt.Errorf("taskboard: %w", &mcpclient.ToolError{Tool: "task_get", Message: "not found"}),
	}}
	if _, err := (&Client{MCP: r}).Get(context.Background(), "T9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
