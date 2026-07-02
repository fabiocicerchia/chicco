package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMessagesNonStreaming confirms a non-streaming Anthropic request gets a
// single Anthropic-shaped JSON response, translated from the OpenAI SSE chicco
// always requests upstream.
func TestMessagesNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("upstream body = %s, want chicco to always request stream:true", body)
		}
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	rot := NewRotator([]Provider{
		{Name: "p", BaseURL: upstream.URL, APIKey: "k", Models: []string{"m"}},
	}, nil)
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", out.Type, out.Role)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "hi" {
		t.Errorf("content = %+v, want one text block %q", out.Content, "hi")
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", out.StopReason)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want input=5 output=2", out.Usage)
	}
}

// TestMessagesStreaming confirms a streaming Anthropic request gets a proper
// Anthropic SSE event sequence back.
func TestMessagesStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	rot := NewRotator([]Provider{
		{Name: "p", BaseURL: upstream.URL, APIKey: "k", Models: []string{"m"}},
	}, nil)
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"text":"hi","type":"text_delta"`,
		"event: content_block_stop",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SSE output missing %q, got:\n%s", want, out)
		}
	}
}

// TestMessagesToolUse confirms an upstream tool_calls delta becomes an
// Anthropic tool_use content block with the accumulated JSON input parsed.
func TestMessagesToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"NYC\\\"}\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	rot := NewRotator([]Provider{
		{Name: "p", BaseURL: upstream.URL, APIKey: "k", Models: []string{"m"}},
	}, nil)
	srv := httptest.NewServer(Handler(rot, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"weather?"}],`+
			`"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Content []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v, want one tool_use block", out.Content)
	}
	b := out.Content[0]
	if b.ID != "call_1" || b.Name != "get_weather" || b.Input["city"] != "NYC" {
		t.Errorf("tool_use block = %+v, want id=call_1 name=get_weather input.city=NYC", b)
	}
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
	}
}

// TestMessagesSharesFailoverState confirms /v1/messages retries the next
// provider on a 429 and blocks the first — the same cooldown state
// /v1/chat/completions uses, proving there's no separate rotation for this
// endpoint.
func TestMessagesSharesFailoverState(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer limited.Close()
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"whatever","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	rot.mu.Lock()
	_, blocked := rot.blocked["a"]
	rot.mu.Unlock()
	if !blocked {
		t.Errorf("provider a not blocked after 429 via /v1/messages")
	}
}

// TestAnthropicToOpenAI covers the request-translation edge cases: a system
// string, a plain-string user turn, and a tool_result block.
func TestAnthropicToOpenAI(t *testing.T) {
	body := []byte(`{
		"model": "claude-x",
		"max_tokens": 50,
		"system": "be terse",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type":"tool_use","id":"call_1","name":"f","input":{"a":1}}]},
			{"role": "user", "content": [{"type":"tool_result","tool_use_id":"call_1","content":"42"}]}
		]
	}`)
	payload, model, stream, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("anthropicToOpenAI: %v", err)
	}
	if model != "claude-x" || stream {
		t.Errorf("model/stream = %q/%v, want claude-x/false", model, stream)
	}
	messages, _ := payload["messages"].([]map[string]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %+v, want 4 (system, user, assistant-with-tool_calls, tool)", messages)
	}
	if messages[0]["role"] != "system" || messages[0]["content"] != "be terse" {
		t.Errorf("messages[0] = %+v, want system/be terse", messages[0])
	}
	if messages[2]["role"] != "assistant" {
		t.Errorf("messages[2] = %+v, want role assistant", messages[2])
	}
	if _, ok := messages[2]["tool_calls"]; !ok {
		t.Errorf("messages[2] = %+v, want a tool_calls field", messages[2])
	}
	if messages[3]["role"] != "tool" || messages[3]["tool_call_id"] != "call_1" || messages[3]["content"] != "42" {
		t.Errorf("messages[3] = %+v, want tool/call_1/42", messages[3])
	}
}
