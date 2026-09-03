package proxy

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

// clisynth.go synthesizes the OpenAI JSON and SSE bodies a CLI provider's plain
// text has to arrive as, so a CLI backend is indistinguishable downstream.

// synthJSON - Renders a completion as a non-streamed OpenAI chat.completion
// object, for callers that sent "stream": false. Carries the fields strict
// clients require (id/object/created/model/finish_reason) rather than the bare
// choices array the SSE path gets away with.
func synthJSON(model, text string, promptTokens, tokens int64) []byte {
	out, _ := json.Marshal(map[string]any{
		"id":      synthID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": synthUsage(promptTokens, tokens),
	})
	return out
}

// synthSSE - Renders a completion as the minimal OpenAI SSE stream a client
// accepts: one content delta, an optional usage chunk (for the dashboard bar),
// and [DONE]. Every chunk carries id/model because they are what a caller reads
// back to tell WHICH provider served it — /v1/messages relays them as the
// Anthropic response's id and model, which were empty for CLI-served replies
// while these were omitted.
func synthSSE(model, text string, promptTokens, tokens int64) []byte {
	var b bytes.Buffer
	id := synthID()
	created := time.Now().Unix()
	chunk, _ := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	})
	b.WriteString("data: ")
	b.Write(chunk)
	b.WriteString("\n\n")
	if tokens > 0 || promptTokens > 0 {
		usage, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   synthUsage(promptTokens, tokens),
		})
		b.WriteString("data: ")
		b.Write(usage)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

// synthID - Mints a response id for a CLI provider, which has none of its own.
// Prefixed so a synthesized id is recognisable as chicco's in a log, and
// nanosecond-based so two replies in the same second do not collide.
func synthID() string {
	return "chatcmpl-chicco-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// synthUsage - Builds the usage block for a CLI provider, which reports no
// token counts. The shape is OpenAI's because everything downstream reads it
// that way; the numbers are estimates, and the rotator treats them as such.
func synthUsage(promptTokens, tokens int64) map[string]any {
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": tokens,
		"total_tokens":      promptTokens + tokens,
	}
}
