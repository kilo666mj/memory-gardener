package mcpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBearerWaitsOutRateLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Extra") != "1" {
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

	client := &http.Client{Transport: &bearer{token: "tok", headers: map[string]string{"X-Extra": "1"}, base: http.DefaultTransport, interval: time.Millisecond, maxWait: time.Second}}
	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 2 {
		t.Fatalf("status %d after %d calls", resp.StatusCode, calls)
	}
}
