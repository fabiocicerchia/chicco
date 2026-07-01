package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCooldown(t *testing.T) {
	cases := []struct {
		status     int
		retryAfter time.Duration
		want       time.Duration
	}{
		{http.StatusUnauthorized, 0, authCooldown},
		{http.StatusForbidden, 0, authCooldown},
		{http.StatusTooManyRequests, 30 * time.Second, 30 * time.Second},
		{http.StatusTooManyRequests, 0, defaultCooldown},
		{http.StatusInternalServerError, 0, defaultCooldown},
	}
	for _, c := range cases {
		if got := cooldown(c.status, c.retryAfter); got != c.want {
			t.Errorf("cooldown(%d, %v) = %v, want %v", c.status, c.retryAfter, got, c.want)
		}
	}
}

// TestRotationFailover verifies a 429 from the first provider rotates onto the
// second, which answers; the response is proxied back and the first provider is
// blocked using its Retry-After hint.
func TestRotationFailover(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer limited.Close()
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer key-b" {
			t.Errorf("Authorization = %q, want Bearer key-b", got)
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer working.Close()

	rot := NewRotator([]Provider{
		{Name: "a", BaseURL: limited.URL, APIKey: "key-a", Models: []string{"m-a"}},
		{Name: "b", BaseURL: working.URL, APIKey: "key-b", Models: []string{"m-b"}},
	}, nil)
	srv := httptest.NewServer(Handler(rot))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"whatever","messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"content":"hi"`) {
		t.Errorf("proxied body = %q, want the working provider's SSE", out)
	}

	rot.mu.Lock()
	until, blocked := rot.blocked["a"]
	rot.mu.Unlock()
	if !blocked || time.Until(until) < 30*time.Second {
		t.Errorf("provider a not blocked with Retry-After cooldown (until=%v)", until)
	}
}

// TestModelOverride confirms the requested model is replaced by the rotation's
// configured model before forwarding upstream.
func TestModelOverride(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"chosen-model"`) {
			gotModel = "chosen-model"
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	rot := NewRotator([]Provider{
		{Name: "p", BaseURL: upstream.URL, APIKey: "k", Models: []string{"chosen-model"}},
	}, nil)
	srv := httptest.NewServer(Handler(rot))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"ignored","messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if gotModel != "chosen-model" {
		t.Errorf("upstream model = %q, want chosen-model (request model not overridden)", gotModel)
	}
}

// TestActiveSkipsUnconfigured drops providers without a key or models.
func TestActiveSkipsUnconfigured(t *testing.T) {
	rot := NewRotator([]Provider{
		{Name: "nokey", BaseURL: "http://x", APIKey: "", Models: []string{"m"}},
		{Name: "nomodel", BaseURL: "http://x", APIKey: "k", Models: nil},
		{Name: "ok", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}},
	}, nil)
	active := rot.Active()
	if len(active) != 1 || active[0].Name != "ok" {
		t.Errorf("Active() = %+v, want only [ok]", active)
	}
}
