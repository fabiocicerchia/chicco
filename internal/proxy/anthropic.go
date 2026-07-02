package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// Anthropic-compatible /v1/messages.
//
// chicco talks OpenAI chat-completions to every provider internally (that's the
// one shape pick/forward/runCLI/dispatch understand). This file translates at the
// edges only: an incoming Anthropic request becomes an OpenAI-shaped
// map[string]any fed into the same dispatch() used by /v1/chat/completions, and
// the OpenAI-shaped upstream reply is translated back into Anthropic's message /
// SSE-event shape. Cooldown, health, quota, and the dashboard are untouched —
// they only ever see the OpenAI shape.

// handleMessages is the Anthropic-compatible sibling of handleChat: same
// rotation, cooldown, and quota machinery (via dispatch), different wire format
// in and out.
func (r *Rotator) handleMessages(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "chicco: read body: "+err.Error())
		return
	}
	payload, requestedModel, wantStream, err := anthropicToOpenAI(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "chicco: invalid Anthropic request: "+err.Error())
		return
	}

	// Always speak SSE upstream regardless of what the Anthropic caller asked
	// for: CLI providers only ever synthesize SSE (see synthSSE in cli.go), and
	// this lets one translator state machine (translateOpenAIStream) feed both a
	// live SSE relay and a buffered single-JSON response.
	payload["stream"] = true
	payload["stream_options"] = map[string]any{"include_usage": true}

	res, err := r.dispatch(req.Context(), requestedModel, payload, "/chat/completions")
	if err != nil {
		writeAnthropicError(w, dispatchStatus(err), "overloaded_error", err.Error())
		return
	}

	var tokens int64
	if wantStream {
		tokens = respondAnthropicStream(w, res.up)
	} else {
		tokens = respondAnthropicJSON(w, res.up)
	}
	r.recordUsage(res.provider, res.model, tokens)
	log.Printf("chicco: %s (%s) served %d tokens (anthropic)", res.provider, res.model, tokens)
}

// writeAnthropicError replies with Anthropic's error envelope
// ({"type":"error","error":{"type","message"}}) rather than OpenAI's.
func writeAnthropicError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": msg},
	})
}

// --- Request translation: Anthropic -> OpenAI-shaped map[string]any ---

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

// anthropicToOpenAI decodes an Anthropic /v1/messages body into an
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

	messages := []map[string]any{}
	if sys := anthropicBlockText(req.System); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		converted, err := convertAnthropicMessage(m)
		if err != nil {
			return nil, "", false, err
		}
		messages = append(messages, converted...)
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
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  json.RawMessage(t.InputSchema),
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

// convertAnthropicMessage turns one Anthropic message (string or content-block
// content) into one or more OpenAI-shaped messages. A user turn's tool_result
// blocks become separate role:"tool" messages (OpenAI's shape); an assistant
// turn's tool_use blocks become one message's tool_calls array.
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

// anthropicStringContent reports whether raw is a plain JSON string (Anthropic
// allows "content" to be either a string or a content-block array).
func anthropicStringContent(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// anthropicBlockText extracts text from a field that may be a plain string, a
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

// anthropicToolChoice maps Anthropic's tool_choice shape to OpenAI's.
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

// anthropicSink receives the logical events translateOpenAIStream extracts from
// an OpenAI SSE stream. sseSink renders them live as Anthropic SSE; jsonSink
// buffers them into one Anthropic response object.
type anthropicSink interface {
	start(id, model string)
	openText()
	textDelta(text string)
	openTool(id, name string)
	toolDelta(partialJSON string)
	closeBlock()
	finish(stopReason string, inputTokens, outputTokens int64)
}

type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// mapStopReason maps an OpenAI finish_reason to Anthropic's stop_reason.
func mapStopReason(openaiReason string) string {
	switch openaiReason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

// translateOpenAIStream reads an upstream OpenAI SSE body (chicco always
// requests one — see handleMessages) line by line and drives sink through the
// equivalent Anthropic event sequence. It also accepts a single non-streamed
// OpenAI JSON body (choices[].message instead of choices[].delta) so a future
// non-SSE upstream still works. Returns the total token count for chicco's own
// usage accounting (independent of what's reported to the Anthropic client).
//
// ponytail: assumes at most one content block is "in flight" at a time (a text
// block, then a tool call, then another tool call, ...) — true of every
// OpenAI-compatible provider chicco talks to today. Genuinely interleaved
// parallel blocks would need per-index open-block tracking; add if a provider
// ever needs it.
func translateOpenAIStream(body io.Reader, sink anthropicSink) int64 {
	br := bufio.NewReaderSize(body, 1024*1024)
	started := false
	openKind := "" // "", "text", "tool"
	openToolIdx := -1
	stopReason := ""
	var inputTokens, outputTokens, totalTokens int64

	closeIfOpen := func() {
		if openKind != "" {
			sink.closeBlock()
			openKind = ""
		}
	}

	for {
		line, rerr := br.ReadBytes('\n')
		data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
		if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) && data[0] == '{' {
			var chunk openAIChunk
			if json.Unmarshal(data, &chunk) == nil {
				if !started {
					sink.start(chunk.ID, chunk.Model)
					started = true
				}
				for _, c := range chunk.Choices {
					content := c.Delta.Content
					toolCalls := c.Delta.ToolCalls
					if content == "" && len(toolCalls) == 0 && c.Message.Content != "" {
						content = c.Message.Content // non-streamed body: choices[].message
					}
					if content != "" {
						if openKind != "text" {
							closeIfOpen()
							sink.openText()
							openKind = "text"
						}
						sink.textDelta(content)
					}
					for _, tc := range toolCalls {
						if openKind != "tool" || openToolIdx != tc.Index {
							closeIfOpen()
							sink.openTool(tc.ID, tc.Function.Name)
							openKind = "tool"
							openToolIdx = tc.Index
						}
						sink.toolDelta(tc.Function.Arguments)
					}
					for _, tc := range c.Message.ToolCalls { // non-streamed body
						closeIfOpen()
						sink.openTool(tc.ID, tc.Function.Name)
						sink.toolDelta(tc.Function.Arguments)
						openKind = "tool"
					}
					if c.FinishReason != "" {
						stopReason = mapStopReason(c.FinishReason)
					}
				}
				if chunk.Usage != nil {
					inputTokens = chunk.Usage.PromptTokens
					outputTokens = chunk.Usage.CompletionTokens
					totalTokens = chunk.Usage.TotalTokens
				}
			}
		}
		if rerr != nil {
			break
		}
	}

	closeIfOpen()
	if stopReason == "" {
		stopReason = "end_turn"
	}
	// CLI providers' synthesized usage only carries a total (see synthSSE in
	// cli.go); attribute it all to output since the whole reply is generated text.
	if outputTokens == 0 && totalTokens > 0 {
		outputTokens = totalTokens
	}
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens
	}
	sink.finish(stopReason, inputTokens, outputTokens)
	return totalTokens
}

// --- sseSink: live Anthropic SSE relay ---

type sseSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	index   int
	openIdx *int
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]any) {
	_, _ = io.WriteString(w, "event: "+event+"\n")
	b, _ := json.Marshal(data)
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(b)
	_, _ = io.WriteString(w, "\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *sseSink) start(id, model string) {
	if id == "" {
		id = "msg_chicco"
	}
	writeSSEEvent(s.w, s.flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant",
			"content": []any{}, "model": model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *sseSink) openText() {
	idx := s.index
	s.index++
	s.openIdx = &idx
	writeSSEEvent(s.w, s.flusher, "content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *sseSink) textDelta(text string) {
	writeSSEEvent(s.w, s.flusher, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": *s.openIdx,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (s *sseSink) openTool(id, name string) {
	idx := s.index
	s.index++
	s.openIdx = &idx
	writeSSEEvent(s.w, s.flusher, "content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
	})
}

func (s *sseSink) toolDelta(partialJSON string) {
	writeSSEEvent(s.w, s.flusher, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": *s.openIdx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partialJSON},
	})
}

func (s *sseSink) closeBlock() {
	if s.openIdx == nil {
		return
	}
	writeSSEEvent(s.w, s.flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": *s.openIdx})
	s.openIdx = nil
}

func (s *sseSink) finish(stopReason string, _, outputTokens int64) {
	writeSSEEvent(s.w, s.flusher, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	})
	writeSSEEvent(s.w, s.flusher, "message_stop", map[string]any{"type": "message_stop"})
}

func respondAnthropicStream(w http.ResponseWriter, up *upstream) int64 {
	defer up.body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return translateOpenAIStream(up.body, &sseSink{w: w, flusher: flusher})
}

// --- jsonSink: buffered into one Anthropic response object ---

type toolAccum struct {
	id, name string
	args     strings.Builder
}

type jsonSink struct {
	id, model                 string
	content                   []map[string]any
	curText                   *strings.Builder
	curTool                   *toolAccum
	stopReason                string
	inputTokens, outputTokens int64
}

func (s *jsonSink) start(id, model string) { s.id, s.model = id, model }
func (s *jsonSink) openText()              { s.curText = &strings.Builder{} }
func (s *jsonSink) textDelta(t string) {
	if s.curText != nil {
		s.curText.WriteString(t)
	}
}
func (s *jsonSink) openTool(id, name string) { s.curTool = &toolAccum{id: id, name: name} }
func (s *jsonSink) toolDelta(p string) {
	if s.curTool != nil {
		s.curTool.args.WriteString(p)
	}
}

func (s *jsonSink) closeBlock() {
	if s.curText != nil {
		s.content = append(s.content, map[string]any{"type": "text", "text": s.curText.String()})
		s.curText = nil
	}
	if s.curTool != nil {
		raw := s.curTool.args.String()
		if raw == "" {
			raw = "{}"
		}
		var input any
		if json.Unmarshal([]byte(raw), &input) != nil {
			input = map[string]any{}
		}
		s.content = append(s.content, map[string]any{"type": "tool_use", "id": s.curTool.id, "name": s.curTool.name, "input": input})
		s.curTool = nil
	}
}

func (s *jsonSink) finish(stopReason string, inputTokens, outputTokens int64) {
	s.stopReason = stopReason
	s.inputTokens, s.outputTokens = inputTokens, outputTokens
}

func respondAnthropicJSON(w http.ResponseWriter, up *upstream) int64 {
	defer up.body.Close()
	sink := &jsonSink{}
	total := translateOpenAIStream(up.body, sink)
	if sink.id == "" {
		sink.id = "msg_chicco"
	}
	if sink.content == nil {
		sink.content = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": sink.id, "type": "message", "role": "assistant",
		"content": sink.content, "model": sink.model,
		"stop_reason": sink.stopReason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": sink.inputTokens, "output_tokens": sink.outputTokens},
	})
	return total
}
