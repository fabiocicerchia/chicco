package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
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
		// Request-shaped 4xx get only a brief skip, even if a bogus Retry-After
		// rides along, so a healthy provider isn't locked out of future requests.
		{http.StatusBadRequest, 0, requestErrorCooldown},
		{http.StatusNotFound, 0, requestErrorCooldown},
		{http.StatusRequestEntityTooLarge, 30 * time.Second, requestErrorCooldown},
		{http.StatusUnprocessableEntity, 0, requestErrorCooldown},
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

// TestEmbeddingsSkipsCLI confirms an embeddings request never lands on a CLI
// provider (which returns chat text, not vectors) even when one sorts first, and
// that the HTTP provider it does reach is hit on /embeddings.
func TestEmbeddingsSkipsCLI(t *testing.T) {
	var gotPath string
	http4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"data":[{"embedding":[0.1]}],"usage":{"total_tokens":3}}`)
	}))
	defer http4.Close()

	rot := NewRotator([]Provider{
		{Name: "cli-first", Kind: "cli", Command: "sh", Args: []string{"-c", "printf notavector"}, Models: []string{"m"}},
		{Name: "http", BaseURL: http4.URL, APIKey: "k", Models: []string{"embed-model"}},
	}, nil)
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/embeddings", "application/json",
		strings.NewReader(`{"model":"chicco:auto","input":"hi"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if gotPath != "/embeddings" {
		t.Errorf("upstream path = %q, want /embeddings (request never reached HTTP provider)", gotPath)
	}
	if !strings.Contains(string(out), `"embedding"`) {
		t.Errorf("body = %q, want the HTTP provider's embedding, not CLI text", out)
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

// TestModelsEndpointHidesDeadModels confirms /v1/models drops a virtual model
// whose every backend is greyed out (bad key, logged-out or missing CLI,
// unreachable endpoint) — listing it only gets a caller to pick it and take a
// 503 — while a model with one live backend, and one merely in cooldown (a rate
// limit that reopens on its own), both stay listed.
func TestModelsEndpointHidesDeadModels(t *testing.T) {
	rot := NewRotator([]Provider{
		{Name: "live", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}},
		{Name: "dead", BaseURL: "http://y", APIKey: "k", Models: []string{"m"}},
		{Name: "nokey", BaseURL: "http://z", APIKey: "k", Models: []string{"m"}},
	}, []Model{
		{ID: "mixed", Backends: []Backend{{Provider: "dead", Model: "m"}, {Provider: "live", Model: "m"}}},
		{ID: "gone", Backends: []Backend{{Provider: "dead", Model: "m"}}},
		{ID: "limited", Backends: []Backend{{Provider: "nokey", Model: "m"}}},
	})
	rot.setHealth("live", HealthOK)
	rot.setHealth("dead", HealthDown)
	rot.setHealth("nokey", HealthOK)
	rot.block("nokey", time.Minute, "rate")

	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got []string
	for _, d := range out.Data {
		got = append(got, d.ID)
	}
	want := []string{"chicco:auto", "mixed", "limited"}
	if !slices.Equal(got, want) {
		t.Errorf("models = %v, want %v", got, want)
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

	// A browser can't send a bearer token: /dashboard?key= trades the secret for
	// a cookie that then authenticates the page and its /v1/status polling.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	resp, err := browser.Get(srv.URL + "/dashboard?key=wrong")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/dashboard?key=wrong = %d, want 401", resp.StatusCode)
	}
	resp, err = browser.Get(srv.URL + "/dashboard?key=s3cret")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/dashboard?key=s3cret = %d, want 200 after the cookie redirect", resp.StatusCode)
	}
	resp, err = browser.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatalf("GET /v1/status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/status with the dashboard cookie = %d, want 200", resp.StatusCode)
	}

	// With no key configured, chicco is open (the localhost default).
	open := NewRotator([]Provider{{Name: "a", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}}}, nil)
	osrv := httptest.NewServer(Handler(open, nil))
	defer osrv.Close()
	resp, err = http.Get(osrv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("open /v1/models = %d, want 200", resp.StatusCode)
	}
}

// TestHealthBody confirms /health names each provider's state instead of
// answering an empty 200, which reported a provider whose key is rejected as
// indistinguishable from a working one. The status stays 200 either way — the
// chart uses this path for liveness.
func TestHealthBody(t *testing.T) {
	rot := NewRotator([]Provider{
		{Name: "good", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}},
		{Name: "bad", BaseURL: "http://y", APIKey: "k", Models: []string{"m"}},
	}, nil)
	rot.setHealth("good", HealthOK)
	rot.setHealth("bad", HealthAuth)
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Status    string            `json:"status"`
		Providers map[string]string `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "ok" || out.Providers["good"] != "ok" || out.Providers["bad"] != "auth" {
		t.Errorf("/health = %+v, want status ok with good=ok bad=auth", out)
	}

	// Every provider unusable → still 200, but the body says degraded.
	rot.setHealth("good", HealthDown)
	resp2, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.StatusCode != http.StatusOK || out.Status != "degraded" {
		t.Errorf("all-down /health = %d %+v, want 200 degraded", resp2.StatusCode, out)
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

// TestClassifyUpstream pins the mapping that caused the worst observed failure
// mode: a 403 meaning "this model isn't on your plan" or "free quota exhausted"
// was treated as a rejected API key, cooling the whole provider for an hour and
// greying it as auth-failed. One wrong model id in the config took five working
// models offline with it.
func TestClassifyUpstream(t *testing.T) {
	for _, c := range []struct {
		name       string
		status     int
		body       string
		wantReason string
		wantScoped bool
		wantAtMost time.Duration // 0 = don't care
	}{
		{"real bad key", 401, `{"error":{"message":"Invalid API key"}}`, "auth", false, 0},
		{"bare 403", 403, `{"error":{"message":"Forbidden"}}`, "auth", false, 0},
		{"cloudflare model not on plan", 403,
			`{"errors":[{"message":"AiError: Model @cf/moonshotai/kimi-k2.7-code is not available on the Workers AI plan"}]}`,
			"error", true, requestErrorCooldown},
		{"alibaba free quota gone", 403,
			`{"error":{"message":"The free quota has been exhausted. To continue accessing the model on a paid basis..."}}`,
			"limit", false, 0},
		{"openrouter out of credits", 402,
			`{"error":{"message":"This request requires more credits, or fewer max_tokens."}}`,
			"limit", false, 0},
		{"groq unknown model", 404,
			`{"error":{"message":"The model ` + "`qwen3-32b`" + ` does not exist or you do not have access to it."}}`,
			"error", true, requestErrorCooldown},
		{"mistral invalid model", 400,
			`{"object":"error","message":"Invalid model: pixtral-large-latest","type":"invalid_model"}`,
			"error", true, requestErrorCooldown},
		{"plain rate limit", 429, `{"error":{"message":"rate limit reached"}}`, "limit", false, 0},
		{"server error", 502, `bad gateway`, "error", false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := classifyUpstream(c.status, c.body, 0)
			if got.reason != c.wantReason {
				t.Errorf("reason = %q, want %q", got.reason, c.wantReason)
			}
			if got.modelScoped != c.wantScoped {
				t.Errorf("modelScoped = %v, want %v (scoped keeps sibling models routable)", got.modelScoped, c.wantScoped)
			}
			if c.wantAtMost > 0 && got.cooldown > c.wantAtMost {
				t.Errorf("cooldown = %v, want <= %v", got.cooldown, c.wantAtMost)
			}
			if !c.wantScoped && c.wantReason != "auth" && got.cooldown == authCooldown && c.status == 403 {
				// a quota 403 may legitimately last an hour, but it must not be
				// labelled "auth" — that is what made the dashboard lie.
				if got.reason == "auth" {
					t.Errorf("403 quota reply still labelled auth")
				}
			}
		})
	}
}

// TestModelScopedBlockKeepsSiblingsRoutable is the behavioural half: a provider
// whose FIRST model is rejected by name must still serve its other models.
func TestModelScopedBlockKeepsSiblingsRoutable(t *testing.T) {
	rot := NewRotator([]Provider{{
		Name: "p", BaseURL: "http://unused", APIKey: "k",
		Models: []string{"bad-model", "good-model"},
	}}, nil)

	// Simulate what dispatch does for a "model not available" 403 on bad-model.
	cls := classifyUpstream(403, `Model bad-model is not available on this plan`, 0)
	if !cls.modelScoped {
		t.Fatal("precondition: expected a model-scoped verdict")
	}
	rot.block("p/bad-model", cls.cooldown, cls.reason)

	active := rot.Active()
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		if _, model, ok := rot.pick(active, "order"); ok {
			seen[model] = true
		}
	}
	if seen["bad-model"] {
		t.Error("pick returned the blocked model")
	}
	if !seen["good-model"] {
		t.Error("sibling model was unreachable — the block leaked to the whole provider")
	}
}
