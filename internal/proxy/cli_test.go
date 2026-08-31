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
	p := Provider{Output: "json", ResultPath: "result", TokensPath: "usage.output_tokens",
		InTokensPath: "usage.input_tokens"}
	text, tokens, in, failed := extractCompletion(p, []byte(`{"result":"done","usage":{"output_tokens":42,"input_tokens":7}}`))
	if text != "done" || tokens != 42 || in != 7 || failed {
		t.Errorf("extractCompletion = %q, %d, %d, %v; want done, 42, 7, false", text, tokens, in, failed)
	}
	// Non-JSON output falls back to raw text.
	pt := Provider{Output: "text"}
	if txt, _, _, _ := extractCompletion(pt, []byte("plain answer")); txt != "plain answer" {
		t.Errorf("text extract = %q", txt)
	}
	// error_path truthy → failed, so the caller can fail over.
	pe := Provider{Output: "json", ResultPath: "result", ErrorPath: "is_error"}
	if _, _, _, failed := extractCompletion(pe, []byte(`{"is_error":true,"result":"Not logged in"}`)); !failed {
		t.Error("expected failed=true when is_error is set")
	}
}

func TestSynthSSEParsesAsOpenAI(t *testing.T) {
	out := string(synthSSE("some-model", "hello world", 4, 12))
	if !strings.Contains(out, `"content":"hello world"`) {
		t.Errorf("missing content delta: %q", out)
	}
	if !strings.Contains(out, `"total_tokens":16`) || !strings.Contains(out, `"prompt_tokens":4`) {
		t.Errorf("missing usage: %q", out)
	}
	// The model must be on the chunks: /v1/messages reads it back as the
	// Anthropic response's "model", which was empty for CLI-served replies.
	if !strings.Contains(out, `"model":"some-model"`) {
		t.Errorf("missing model: %q", out)
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
	if got != 16 {
		t.Errorf("usageTokens round-trip = %d, want 16", got)
	}
}

// TestRunCLIEndToEnd drives a CLI provider through the full handler using a real
// subprocess (sh) that echoes a fixed answer, and asserts chicco answers in the
// shape the caller asked for: a chat.completion object by default, SSE only when
// the request set "stream": true. Synthesizing SSE unconditionally handed a
// non-streaming client text/event-stream, which no OpenAI client parses.
func TestRunCLIEndToEnd(t *testing.T) {
	newRot := func() *Rotator {
		return NewRotator([]Provider{{
			Name:    "fake-cli",
			Kind:    "cli",
			Command: "sh",
			Args:    []string{"-c", "printf 'hello from {{model}}'"},
			Models:  []string{"m1"},
		}}, nil)
	}

	for _, c := range []struct {
		name, body, wantCT string
		wantSSE            bool
	}{
		{"stream omitted", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`, "application/json", false},
		{"stream false", `{"model":"x","stream":false,"messages":[{"role":"user","content":"hi"}]}`, "application/json", false},
		{"stream true", `{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "text/event-stream", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rot := newRot()
			srv := httptest.NewServer(Handler(rot, nil))
			defer srv.Close()

			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(c.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if ct := resp.Header.Get("Content-Type"); ct != c.wantCT {
				t.Errorf("Content-Type = %q, want %q", ct, c.wantCT)
			}
			if !strings.Contains(string(body), `hello from m1`) {
				t.Errorf("CLI output not proxied: %q", body)
			}
			if got := strings.Contains(string(body), "[DONE]"); got != c.wantSSE {
				t.Errorf("SSE framing = %v, want %v: %q", got, c.wantSSE, body)
			}
			if !c.wantSSE && !strings.Contains(string(body), `"object":"chat.completion"`) {
				t.Errorf("non-streaming reply is not a chat.completion object: %q", body)
			}
			// A served request is recorded with an estimated token count.
			if s := rot.Snapshot(); s[0].Requests != 1 || s[0].UsedTokens == 0 {
				t.Errorf("usage not recorded: %+v", s[0])
			}
		})
	}
}

// TestRunCLIViaAnthropicEndpointGetsThePrompt pins the bug that made every CLI
// provider unusable through /v1/messages: anthropicToOpenAI built messages as
// []map[string]any while splitMessages asserts .([]any), so the assertion failed
// silently and the tool ran with an EMPTY prompt. The fake CLI echoes its own
// argv, so an empty prompt is visible in the reply.
func TestRunCLIViaAnthropicEndpointGetsThePrompt(t *testing.T) {
	rot := NewRotator([]Provider{{
		Name:    "echo-cli",
		Kind:    "cli",
		Command: "sh",
		Args:    []string{"-c", `printf 'PROMPT<%s>' "$1"`, "sh", "{{prompt}}"},
		Models:  []string{"m1"},
	}}, nil)
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m1","max_tokens":16,"system":"be terse","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "PROMPT<>") {
		t.Fatalf("CLI provider ran with an EMPTY prompt via /v1/messages: %s", body)
	}
	for _, want := range []string{"ping", "be terse"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("prompt did not reach the CLI (missing %q): %s", want, body)
		}
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
	srv := httptest.NewServer(Handler(rot, nil))
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

// TestToolsSkipCLIProviders pins the fix for a request that carries tool
// definitions: a CLI provider answers it with the call narrated in prose, which
// an agent reads as "did nothing". It must be routed past, and the request must
// fail loudly when no other provider can take it.
func TestToolsSkipCLIProviders(t *testing.T) {
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer working.Close()

	cli := Provider{Name: "cli", Kind: "cli", Command: "sh", Args: []string{"-c", "printf narrated"}, Models: []string{"m"}}
	payload := func() map[string]any {
		return map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools":    []any{map[string]any{"type": "function"}},
		}
	}

	// The CLI provider is first in config order, so plain rotation would pick it.
	rot := NewRotator([]Provider{cli, {Name: "http", BaseURL: working.URL, APIKey: "k", Models: []string{"m"}}}, nil)
	res, err := rot.dispatch(context.Background(), "chicco:auto", payload(), "/chat/completions")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res.up.body.Close()
	if res.provider != "http" {
		t.Errorf("tools request served by %q, want the HTTP provider", res.provider)
	}

	// Nothing but CLI providers: a 503 saying why beats a narrated non-answer.
	only := NewRotator([]Provider{cli}, nil)
	_, err = only.dispatch(context.Background(), "chicco:auto", payload(), "/chat/completions")
	if err == nil {
		t.Fatal("CLI-only tools request succeeded, want 503")
	}
	if got := dispatchStatus(err); got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", got)
	}
	if !strings.Contains(err.Error(), "tools") {
		t.Errorf("error does not explain the tools gap: %v", err)
	}
	// No Retry-After here: this 503 is a config that cannot serve the request,
	// not a cooldown, and telling a client to come back later would be a lie.
	rec := httptest.NewRecorder()
	setRetryAfter(rec, err)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on a misconfiguration 503, want none", got)
	}
}

func TestProbeCLI(t *testing.T) {
	ctx := context.Background()
	// A CLI that isn't installed here is down, not healthy: it used to probe
	// green off its mounted credential file and only fail at request time.
	notInstalled := Provider{Kind: "cli", Command: "chicco-no-such-cli", Credential: "/"}
	if got, _ := probeCLI(ctx, notInstalled); got != HealthDown {
		t.Errorf("missing binary = %v, want HealthDown", got)
	}
	if got, _ := probeCLI(ctx, Provider{Kind: "cli", HealthCommand: []string{"true"}}); got != HealthOK {
		t.Errorf("health_command true = %v, want HealthOK", got)
	}
	// A status command that exits non-zero means logged out → grey (auth).
	if got, _ := probeCLI(ctx, Provider{Kind: "cli", HealthCommand: []string{"false"}}); got != HealthAuth {
		t.Errorf("health_command false = %v, want HealthAuth", got)
	}
	if got, _ := probeCLI(ctx, Provider{Kind: "cli", Credential: "/no/such/file"}); got != HealthAuth {
		t.Errorf("missing credential = %v, want HealthAuth", got)
	}
	// health_expect: output must contain the marker, else auth (logged out).
	logged := Provider{Kind: "cli", HealthCommand: []string{"sh", "-c", `echo '{"loggedIn": true}'`}, HealthExpect: `"loggedIn": true`}
	if got, _ := probeCLI(ctx, logged); got != HealthOK {
		t.Errorf("health_expect matched = %v, want HealthOK", got)
	}
	out := Provider{Kind: "cli", HealthCommand: []string{"sh", "-c", `echo '{"loggedIn": false}'`}, HealthExpect: `"loggedIn": true`}
	if got, _ := probeCLI(ctx, out); got != HealthAuth {
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

// TestCLIFailureKeepsTheReason uses gemini-cli's real output: a warning line
// first, then a long stack trace, with the actual cause ("Error authenticating")
// in the middle and repeated at the end. Reported head-first and capped at 512
// bytes, all the caller ever saw was the harmless warning — and the auth error,
// which names no login, classified as a transient 502 and was retried every
// minute forever instead of greying the provider out.
func TestCLIFailureKeepsTheReason(t *testing.T) {
	msg := "Approval mode overridden by --approval-mode plan\n" +
		strings.Repeat("    at throwIneligibleOrProjectIdError (file:///home/x/bundle/chunk.js:310030:11)\n", 12) +
		"An unexpected critical error occurred: IneligibleTierError: This client is no longer supported"

	up := cliFailure(msg)
	if up.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (auth → grey, long cooldown)", up.status)
	}
	body, _ := io.ReadAll(io.LimitReader(up.body, cliErrSnippet))
	if !strings.Contains(string(body), "no longer supported") {
		t.Errorf("reported body lost the reason it failed: %q", body)
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
