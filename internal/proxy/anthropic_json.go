package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// anthropic_json.go is the buffering sink: it accumulates the same event
// sequence into the one Anthropic message object a non-streaming caller wants.

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

func (s *jsonSink) openText() { s.curText = &strings.Builder{} }

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

// respondAnthropicJSON - Buffers an upstream OpenAI stream into a single
// Anthropic message object, returning the token total for accounting. The
// upstream is streamed either way; only the client-facing shape differs.
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
