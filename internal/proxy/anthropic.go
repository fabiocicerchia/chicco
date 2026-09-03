package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// Anthropic-compatible /v1/messages.
//
// chicco talks OpenAI chat-completions to every provider internally (that's the
// one shape pick/forward/runCLI/dispatch understand). The anthropic_* files
// translate at the edges only: an incoming Anthropic request becomes an
// OpenAI-shaped map[string]any fed into the same dispatch() used by
// /v1/chat/completions (anthropic_request.go), and the OpenAI-shaped upstream
// reply is translated back into Anthropic's message / SSE-event shape
// (anthropic_stream.go, through the sink in anthropic_sse.go or
// anthropic_json.go). Cooldown, health, quota, and the dashboard are untouched
// — they only ever see the OpenAI shape.

// handleMessages - Is the Anthropic-compatible sibling of handleChat: same
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
		setRetryAfter(w, err)
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
	// Only the total is available on this path — the translator sums the
	// stream — so the cost is priced at the output rate, which is the safe
	// direction. See costTracker.cost.
	log.Printf("chicco: %s (%s) served %d tokens (anthropic)%s", res.provider, res.model, tokens,
		r.costNote(res.provider, res.model, Usage{Total: tokens}))
}

// writeAnthropicError - Replies with Anthropic's error envelope
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
