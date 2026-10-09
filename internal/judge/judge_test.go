package judge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJudgeSendsSchemaAndParsesVerdict(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		content := `{"verdict":"needs_update","reason":"commit abc renamed the flag","replacement":"new text"}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/v1", Model: "m"}
	v, err := c.Judge(context.Background(), Request{MemoryID: "01X", Content: "old text", Signals: []string{"s"}, Evidence: []string{"diff"}, Today: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != NeedsUpdate || v.Replacement != "new text" {
		t.Errorf("verdict = %+v", v)
	}
	rf, _ := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("response_format = %v", got["response_format"])
	}
	msgs, _ := got["messages"].([]any)
	user, _ := msgs[1].(map[string]any)
	if s, _ := user["content"].(string); !strings.Contains(s, "old text") || !strings.Contains(s, "<evidence>\ndiff") {
		t.Errorf("user prompt = %q", s)
	}
}

func TestJudgeUnavailable(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1/v1", Model: "m", HTTP: &http.Client{Timeout: time.Second}}
	if _, err := c.Judge(context.Background(), Request{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading model", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c.BaseURL = srv.URL
	if _, err := c.Judge(context.Background(), Request{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"```json\n{\"verdict\":\"still_true\",\"reason\":\"r\",\"replacement\":\"ignored\"}\n```", StillTrue, false},
		{"<think>hmm</think>{\"verdict\":\"obsolete\",\"reason\":\"r\",\"replacement\":\"\"}", Obsolete, false},
		{`{"verdict":"needs_update","reason":"r","replacement":"  "}`, "", true},
		{`{"verdict":"maybe","reason":"r","replacement":""}`, "", true},
		{"not json", "", true},
	}
	for _, tt := range tests {
		v, err := parse(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parse(%q) err = %v", tt.in, err)
			continue
		}
		if err == nil && (v.Verdict != tt.want || v.Replacement != "") {
			t.Errorf("parse(%q) = %+v", tt.in, v)
		}
	}
}
