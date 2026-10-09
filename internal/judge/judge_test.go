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
		content := `{"verdict":"needs_update","confidence":"high","reason":"commit abc renamed the flag","edits":[{"find":"flag -old","replace":"flag -new"}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/v1", Model: "m"}
	today := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	v, err := c.Judge(context.Background(), Request{MemoryID: "01X", Content: "old text: pass flag -old.", Signals: []string{"s"}, Evidence: []string{"diff"}, Today: today})
	if err != nil {
		t.Fatal(err)
	}
	if want := "old text: pass flag -new.\n\n(Corrected 2026-10-09 by memory-gardener review.)"; v.Verdict != NeedsUpdate || v.Confidence != High || v.Replacement != want {
		t.Errorf("verdict = %+v", v)
	}
	rf, _ := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("response_format = %v", got["response_format"])
	}
	msgs, _ := got["messages"].([]any)
	user, _ := msgs[1].(map[string]any)
	if s, _ := user["content"].(string); !strings.Contains(s, "flag -old") || !strings.Contains(s, "<evidence>\ndiff") {
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
		in             string
		want, wantConf string
		wantErr        bool
	}{
		{"```json\n{\"verdict\":\"still_true\",\"confidence\":\"high\",\"reason\":\"r\",\"edits\":[{\"find\":\"a\",\"replace\":\"b\"}]}\n```", StillTrue, High, false},
		{"<think>hmm</think>{\"verdict\":\"obsolete\",\"confidence\":\"sure\",\"reason\":\"r\",\"edits\":[]}", Obsolete, Low, false},
		{`{"verdict":"maybe","confidence":"high","reason":"r","edits":[]}`, "", "", true},
		{"not json", "", "", true},
	}
	for _, tt := range tests {
		v, err := parse(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parse(%q) err = %v", tt.in, err)
			continue
		}
		if err == nil && (v.Verdict != tt.want || v.Confidence != tt.wantConf || v.Edits != nil) {
			t.Errorf("parse(%q) = %+v", tt.in, v)
		}
	}
}

func TestApply(t *testing.T) {
	content := "Uses port 8080. Runs on host a. Uses port 8080 twice."
	if _, err := Apply(content, []Edit{{Find: "Uses port 8080", Replace: "Uses port 9090"}}); err == nil {
		t.Error("ambiguous find accepted")
	}
	if _, err := Apply(content, []Edit{{Find: "Runs on host b.", Replace: "x"}}); err == nil {
		t.Error("missing find accepted")
	}
	if _, err := Apply(content, nil); err == nil {
		t.Error("no edits accepted")
	}
	got, err := Apply(content, []Edit{{Find: "Runs on host a.", Replace: "Runs on host b."}, {Find: " Uses port 8080 twice.", Replace: ""}})
	if err != nil || got != "Uses port 8080. Runs on host b." {
		t.Errorf("Apply = %q, %v", got, err)
	}
}

func TestUnmatchedEditsBecomeUnsure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{"verdict":"needs_update","confidence":"high","reason":"r","edits":[{"find":"(same content as original memory...)","replace":"x"}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	defer srv.Close()
	v, err := (&Client{BaseURL: srv.URL, Model: "m"}).Judge(context.Background(), Request{Content: "real memory text", Today: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != Unsure || v.Replacement != "" || !strings.Contains(v.Reason, "did not match") {
		t.Errorf("verdict = %+v", v)
	}
}
