package proxy

import (
	"encoding/json"
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
	srv := httptest.NewServer(Handler(rot, nil))
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
	srv := httptest.NewServer(Handler(rot, nil))
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

// TestModelsEndpoint confirms GET /v1/models lists "chicco:auto" plus one entry
// per virtual model from the routing table, in OpenAI list shape.
func TestModelsEndpoint(t *testing.T) {
	rot := NewRotator([]Provider{
		{Name: "a", BaseURL: "http://x", APIKey: "k", Models: []string{"m1", "m2"}},
		{Name: "b", BaseURL: "http://x", APIKey: "k", Models: []string{"m3"}},
	}, []Model{{ID: "fast"}, {ID: "smart"}})
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "list" || len(out.Data) != 3 {
		t.Fatalf("models = %+v, want chicco:auto + 2 virtual models", out)
	}
	want := []string{"chicco:auto", "fast", "smart"}
	for i, id := range want {
		if out.Data[i].ID != id || out.Data[i].Object != "model" {
			t.Errorf("model[%d] = %+v, want id=%q object=model", i, out.Data[i], id)
		}
	}
}

// TestInboundAuth confirms the optional shared secret guards every endpoint
// except /health, and constant-time-compares the presented bearer token.
func TestInboundAuth(t *testing.T) {
	rot := NewRotator([]Provider{{Name: "a", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}}}, nil)
	rot.authKey = "s3cret"
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	get := func(path, auth string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := get("/v1/models", ""); got != http.StatusUnauthorized {
		t.Errorf("/v1/models without key = %d, want 401", got)
	}
	if got := get("/v1/models", "Bearer wrong"); got != http.StatusUnauthorized {
		t.Errorf("/v1/models with wrong key = %d, want 401", got)
	}
	if got := get("/v1/models", "Bearer s3cret"); got != http.StatusOK {
		t.Errorf("/v1/models with right key = %d, want 200", got)
	}
	if got := get("/health", ""); got != http.StatusOK {
		t.Errorf("/health without key = %d, want 200 (probes stay open)", got)
	}

	// With no key configured, chicco is open (the localhost default).
	open := NewRotator([]Provider{{Name: "a", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}}}, nil)
	osrv := httptest.NewServer(Handler(open, nil))
	defer osrv.Close()
	resp, err := http.Get(osrv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("open /v1/models = %d, want 200", resp.StatusCode)
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

// TestGlobalQuotaCapsAcrossProviders confirms a top-level quota trips even
// though the single configured provider has no quota of its own — proving the
// global cap, not a per-provider one, is what stops the third request.
func TestGlobalQuotaCapsAcrossProviders(t *testing.T) {
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer working.Close()

	rot := NewRotator([]Provider{
		{Name: "a", BaseURL: working.URL, APIKey: "key-a", Models: []string{"m-a"}},
	}, nil)
	rot.quota = Quota{RPD: 2}
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	post := func() int {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"whatever","messages":[]}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(); got != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", got)
	}
	if got := post(); got != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200", got)
	}
	if got := post(); got != http.StatusServiceUnavailable {
		t.Errorf("request 3 status = %d, want 503 (global RPD:2 cap tripped)", got)
	}
}
