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
