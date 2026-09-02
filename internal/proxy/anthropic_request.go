package proxy

import (
	"encoding/json"
	"errors"
	"strings"
)

// anthropic_request.go translates an Anthropic /v1/messages request into the
// OpenAI chat-completions payload the rotation forwards.

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

// anthropicToOpenAI - Decodes an Anthropic /v1/messages body into an
// OpenAI-shaped payload ready for dispatch(), plus the requested model and
// whether the caller wants a streamed reply.
func anthropicToOpenAI(body []byte) (payload map[string]any, model string, stream bool, err error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", false, err
	}
	if len(req.Messages) == 0 {
		return nil, "", false, errors.New("messages is required")
	}

	// []any, NOT []map[string]any: this payload is consumed by the same code that
	// handles /v1/chat/completions, where it arrives from json.Unmarshal and so is
	// always []any. splitMessages (cli.go) type-asserts .([]any) — a
	// []map[string]any fails that assertion silently, yielding an EMPTY prompt, so
	// every CLI provider reached through /v1/messages ran with no input at all.
	messages := []any{}
	if sys := anthropicBlockText(req.System); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		converted, err := convertAnthropicMessage(m)
		if err != nil {
			return nil, "", false, err
		}
		for _, c := range converted {
			messages = append(messages, c)
		}
	}

	payload = map[string]any{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": req.MaxTokens,
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		// []any for the same reason as messages above — dispatch() checks
		// payload["tools"].([]any) to warn that a CLI provider ignores
		// function-calling, and a []map[string]any silently skips that warning.
		tools := make([]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.InputSchema,
				},
			}
		}
		payload["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		payload["tool_choice"] = anthropicToolChoice(req.ToolChoice)
	}

	return payload, req.Model, req.Stream, nil
}

// convertAnthropicMessage - Turns one Anthropic message (string or
// content-block content) into one or more OpenAI-shaped messages. A user turn's
// tool_result blocks become separate role:"tool" messages (OpenAI's shape); an
// assistant turn's tool_use blocks become one message's tool_calls array.
func convertAnthropicMessage(m anthropicMessage) ([]map[string]any, error) {
	if s, ok := anthropicStringContent(m.Content); ok {
		return []map[string]any{{"role": m.Role, "content": s}}, nil
	}

	var blocks []anthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, err
	}

	if m.Role == "assistant" {
		var text strings.Builder
		var toolCalls []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool_use":
				toolCalls = append(toolCalls, map[string]any{
					"id":   b.ID,
					"type": "function",
					"function": map[string]any{
						"name":      b.Name,
						"arguments": string(b.Input),
					},
				})
			}
		}
		msg := map[string]any{"role": "assistant", "content": text.String()}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return []map[string]any{msg}, nil
	}

	// role == "user": text blocks accumulate into a user message, flushed
	// whenever a tool_result interrupts them, preserving block order.
	var out []map[string]any
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, map[string]any{"role": "user", "content": text.String()})
			text.Reset()
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(b.Text)
		case "tool_result":
			flush()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      anthropicBlockText(b.Content),
			})
		}
	}
	flush()
	return out, nil
}

// anthropicStringContent - Reports whether raw is a plain JSON string
// (Anthropic allows "content" to be either a string or a content-block array).
func anthropicStringContent(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// anthropicBlockText - Extracts text from a field that may be a plain string, a
// content-block array (joining any text blocks), or absent — used for both
// "system" and a tool_result's "content".
func anthropicBlockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s, ok := anthropicStringContent(raw); ok {
		return s
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var text strings.Builder
	for _, b := range blocks {
		if b.Type == "text" || b.Type == "" {
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

// anthropicToolChoice - Maps Anthropic's tool_choice shape to OpenAI's.
func anthropicToolChoice(raw json.RawMessage) any {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "auto"
	}
	switch tc.Type {
	case "any":
		return "required"
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	default:
		return "auto"
	}
}

// --- Response translation: OpenAI SSE -> Anthropic message / SSE events ---
