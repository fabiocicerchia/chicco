package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// anthropic_stream.go reads an OpenAI SSE stream and drives an anthropicSink
// through the Anthropic event sequence. The sink decides where the events go.

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

// mapStopReason - Maps an OpenAI finish_reason to Anthropic's stop_reason.
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

// translateOpenAIStream - Reads an upstream OpenAI SSE body (chicco always
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
	st := &streamState{sink: sink, openToolIdx: -1}
	for {
		line, rerr := br.ReadBytes('\n')
		if chunk, ok := parseChunk(line); ok {
			st.apply(chunk)
		}
		if rerr != nil {
			break
		}
	}
	return st.finish()
}

// parseChunk - Decodes one SSE line into an OpenAI chunk. ok is false for the
// blank separator lines, the terminating "[DONE]" sentinel and anything that
// isn't a JSON object — all of which the caller skips.
func parseChunk(line []byte) (openAIChunk, bool) {
	data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || data[0] != '{' {
		return openAIChunk{}, false
	}
	var chunk openAIChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return openAIChunk{}, false
	}
	return chunk, true
}

// streamState is the block currently in flight while translating one stream,
// plus the usage totals seen so far. Anthropic's protocol is a sequence of
// opened and closed content blocks, so which block is open — and for a tool
// call, at which choice index — is what decides whether the next delta extends
// the block or starts a new one.
type streamState struct {
	sink        anthropicSink
	started     bool
	openKind    string // "", "text", "tool"
	openToolIdx int
	stopReason  string

	inputTokens  int64
	outputTokens int64
	totalTokens  int64
}

// closeIfOpen - Closes the block in flight, if there is one.
func (s *streamState) closeIfOpen() {
	if s.openKind != "" {
		s.sink.closeBlock()
		s.openKind = ""
	}
}

// apply - Folds one decoded chunk into the state, emitting the Anthropic events
// it implies.
func (s *streamState) apply(chunk openAIChunk) {
	if !s.started {
		s.sink.start(chunk.ID, chunk.Model)
		s.started = true
	}
	s.applyChoices(chunk)
	if chunk.Usage != nil {
		s.inputTokens = chunk.Usage.PromptTokens
		s.outputTokens = chunk.Usage.CompletionTokens
		s.totalTokens = chunk.Usage.TotalTokens
	}
}

// applyChoices - Emits the events for a chunk's choices. It reads both the
// streaming shape (choices[].delta) and the single-object shape
// (choices[].message), so a non-SSE upstream still translates.
func (s *streamState) applyChoices(chunk openAIChunk) {
	for _, c := range chunk.Choices {
		content := c.Delta.Content
		if content == "" && len(c.Delta.ToolCalls) == 0 && c.Message.Content != "" {
			content = c.Message.Content // non-streamed body: choices[].message
		}
		if content != "" {
			s.textDelta(content)
		}
		for _, tc := range c.Delta.ToolCalls {
			s.toolDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		for _, tc := range c.Message.ToolCalls { // non-streamed body
			// Always a fresh block: a non-streamed body carries each tool call
			// whole, so there is nothing to append to.
			s.closeIfOpen()
			s.sink.openTool(tc.ID, tc.Function.Name)
			s.sink.toolDelta(tc.Function.Arguments)
			s.openKind = "tool"
		}
		if c.FinishReason != "" {
			s.stopReason = mapStopReason(c.FinishReason)
		}
	}
}

// textDelta - Appends text to the open text block, opening one first when the
// block in flight is something else.
func (s *streamState) textDelta(content string) {
	if s.openKind != "text" {
		s.closeIfOpen()
		s.sink.openText()
		s.openKind = "text"
	}
	s.sink.textDelta(content)
}

// toolDelta - Appends partial JSON arguments to the open tool block, opening a
// new one when the choice index moved on to the next tool call.
func (s *streamState) toolDelta(index int, id, name, args string) {
	if s.openKind != "tool" || s.openToolIdx != index {
		s.closeIfOpen()
		s.sink.openTool(id, name)
		s.openKind = "tool"
		s.openToolIdx = index
	}
	s.sink.toolDelta(args)
}

// finish - Closes the stream out and returns the total token count for chicco's
// own usage accounting.
func (s *streamState) finish() int64 {
	s.closeIfOpen()
	if s.stopReason == "" {
		s.stopReason = "end_turn"
	}
	// CLI providers' synthesized usage only carries a total (see synthSSE in
	// cli.go); attribute it all to output since the whole reply is generated text.
	if s.outputTokens == 0 && s.totalTokens > 0 {
		s.outputTokens = s.totalTokens
	}
	if s.totalTokens == 0 {
		s.totalTokens = s.inputTokens + s.outputTokens
	}
	s.sink.finish(s.stopReason, s.inputTokens, s.outputTokens)
	return s.totalTokens
}

// --- sseSink: live Anthropic SSE relay ---
