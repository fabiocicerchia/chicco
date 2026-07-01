package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSplitMessages(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "be terse"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
		map[string]any{"role": "user", "content": "again"},
	}}
	sys, user := splitMessages(payload)
	if sys != "be terse" {
		t.Errorf("system = %q", sys)
	}
	if user != "hello\n\nhi\n\nagain" {
		t.Errorf("user = %q", user)
	}
}

func TestDotGetAndExtract(t *testing.T) {
	p := Provider{Output: "json", ResultPath: "result", TokensPath: "usage.output_tokens"}
	text, tokens, failed := extractCompletion(p, []byte(`{"result":"done","usage":{"output_tokens":42}}`))
	if text != "done" || tokens != 42 || failed {
		t.Errorf("extractCompletion = %q, %d, %v; want done, 42, false", text, tokens, failed)
	}
	// Non-JSON output falls back to raw text.
	pt := Provider{Output: "text"}
	if txt, _, _ := extractCompletion(pt, []byte("plain answer")); txt != "plain answer" {
		t.Errorf("text extract = %q", txt)
	}
	// error_path truthy → failed, so the caller can fail over.
	pe := Provider{Output: "json", ResultPath: "result", ErrorPath: "is_error"}
	if _, _, failed := extractCompletion(pe, []byte(`{"is_error":true,"result":"Not logged in"}`)); !failed {
		t.Error("expected failed=true when is_error is set")
	}
}

func TestSynthSSEParsesAsOpenAI(t *testing.T) {
	out := string(synthSSE("hello world", 12))
	if !strings.Contains(out, `"content":"hello world"`) {
		t.Errorf("missing content delta: %q", out)
	}
	if !strings.Contains(out, `"total_tokens":12`) {
		t.Errorf("missing usage: %q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("missing terminator: %q", out)
	}
	// usageTokens (the proxy's own parser) must read our usage chunk back.
	var got int64
	for _, line := range strings.Split(out, "\n") {
		if t := usageTokens([]byte(line)); t > 0 {
			got = t
		}
	}
	if got != 12 {
		t.Errorf("usageTokens round-trip = %d, want 12", got)
	}
}

// TestRunCLIEndToEnd drives a CLI provider through the full handler using a real
// subprocess (sh) that echoes a fixed answer, and asserts chicco proxies it back
// as OpenAI SSE.
func TestRunCLIEndToEnd(t *testing.T) {
	rot := NewRotator([]Provider{{
		Name:    "fake-cli",
		Kind:    "cli",
		Command: "sh",
		Args:    []string{"-c", "printf 'hello from {{model}}'"},
		Models:  []string{"m1"},
	}}, nil)
	srv := httptest.NewServer(Handler(rot))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `hello from m1`) {
		t.Errorf("CLI output not proxied: %q", body)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("missing [DONE]: %q", body)
	}
	// A served request is recorded with an estimated token count.
	if s := rot.Snapshot(); s[0].Requests != 1 || s[0].UsedTokens == 0 {
		t.Errorf("usage not recorded: %+v", s[0])
	}
}

// TestRunCLIFailureFailsOver confirms a non-zero exit cools the CLI provider down
// and the request rotates to the next (working) provider.
func TestRunCLIFailureFailsOver(t *testing.T) {
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer working.Close()

	rot := NewRotator([]Provider{
		{Name: "broken-cli", Kind: "cli", Command: "sh", Args: []string{"-c", "exit 1"}, Models: []string{"m"}},
		{Name: "http", BaseURL: working.URL, APIKey: "k", Models: []string{"m"}},
	}, nil)
	srv := httptest.NewServer(Handler(rot))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"content":"ok"`) {
		t.Errorf("did not fail over to the working provider: %q", body)
	}
	rot.mu.Lock()
	_, blocked := rot.blocked["broken-cli"]
	rot.mu.Unlock()
	if !blocked {
		t.Error("broken CLI provider was not put in cooldown")
	}
}

func TestProbeCLI(t *testing.T) {
	ctx := context.Background()
	if got := probeCLI(ctx, Provider{Kind: "cli", HealthCommand: []string{"true"}}); got != HealthOK {
		t.Errorf("health_command true = %v, want HealthOK", got)
	}
	// A status command that exits non-zero means logged out → grey (auth).
	if got := probeCLI(ctx, Provider{Kind: "cli", HealthCommand: []string{"false"}}); got != HealthAuth {
		t.Errorf("health_command false = %v, want HealthAuth", got)
	}
	if got := probeCLI(ctx, Provider{Kind: "cli", Credential: "/no/such/file"}); got != HealthAuth {
		t.Errorf("missing credential = %v, want HealthAuth", got)
	}
	// health_expect: output must contain the marker, else auth (logged out).
	logged := Provider{Kind: "cli", HealthCommand: []string{"sh", "-c", `echo '{"loggedIn": true}'`}, HealthExpect: `"loggedIn": true`}
	if got := probeCLI(ctx, logged); got != HealthOK {
		t.Errorf("health_expect matched = %v, want HealthOK", got)
	}
	out := Provider{Kind: "cli", HealthCommand: []string{"sh", "-c", `echo '{"loggedIn": false}'`}, HealthExpect: `"loggedIn": true`}
	if got := probeCLI(ctx, out); got != HealthAuth {
		t.Errorf("health_expect missing = %v, want HealthAuth", got)
	}
}

func TestCLIFailureClassifies(t *testing.T) {
	if cliFailure("Not logged in · Please run /login").status != http.StatusUnauthorized {
		t.Error("a logged-out message should classify as 401 (auth → grey)")
	}
	if cliFailure("invalid api key").status != http.StatusUnauthorized {
		t.Error("an invalid-key message should classify as 401")
	}
	if cliFailure("dial tcp: connection refused").status != http.StatusBadGateway {
		t.Error("a transport error should classify as 502 (transient cooldown)")
	}
	// A usage-limit hit → 429 with the parsed reset time as the cooldown.
	up := cliFailure("5-hour limit reached ∙ resets in 2h 30m")
	if up.status != http.StatusTooManyRequests {
		t.Fatalf("limit message status = %d, want 429", up.status)
	}
	if up.retryAfter != 2*time.Hour+30*time.Minute {
		t.Errorf("retryAfter = %v, want 2h30m", up.retryAfter)
	}
	// Limit with no parseable time falls back to the default window cooldown.
	if up := cliFailure("You have reached your usage limit."); up.retryAfter != rateLimitCooldown {
		t.Errorf("fallback cooldown = %v, want %v", up.retryAfter, rateLimitCooldown)
	}
}

func TestParseResetDuration(t *testing.T) {
	cases := []struct {
		msg  string
		want time.Duration
	}{
		{"try again in 45 minutes", 45 * time.Minute},
		{"resets in 1h", time.Hour},
		{"available again in 30 seconds", 30 * time.Second},
		{"5-hour limit reached, resets in 2h", 2 * time.Hour}, // ignores the window length
		{"rate limited", 0},
	}
	for _, c := range cases {
		if got := parseResetDuration(c.msg); got != c.want {
			t.Errorf("parseResetDuration(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
	// "resets at <clock>" yields a positive duration up to 24h out.
	if d := parseResetDuration("usage limit, resets at 3pm"); d <= 0 || d > 24*time.Hour {
		t.Errorf("clock reset = %v, want a positive sub-24h duration", d)
	}
}
