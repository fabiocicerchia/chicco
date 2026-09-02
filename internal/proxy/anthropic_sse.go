package proxy

import (
	"encoding/json"
	"io"
	"net/http"
)

// anthropic_sse.go is the streaming sink: it writes the Anthropic event
// sequence straight to the client as Server-Sent Events.

type sseSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	index   int
	openIdx *int
}

// writeSSEEvent - Writes one Anthropic SSE frame and flushes it. Flushing per
// event is the whole point of a stream: a buffered relay would deliver the
// reply in one lump and the caller could not tell it from a non-streaming one.
// Write errors are ignored deliberately — the client going away mid-stream is
// ordinary, and there is no second channel to report it on.
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

// respondAnthropicStream - Relays an upstream OpenAI stream to the client as
// Anthropic SSE, returning the token total for accounting. Headers go out
// before the first frame, so a failure after this point cannot become a status
// code — which is why dispatch has to have succeeded first.
func respondAnthropicStream(w http.ResponseWriter, up *upstream) int64 {
	defer up.body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return translateOpenAIStream(up.body, &sseSink{w: w, flusher: flusher})
}

// --- jsonSink: buffered into one Anthropic response object ---
