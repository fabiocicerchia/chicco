package proxy

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// dispatch.go runs one request through the rotation: pick, send, relay or fail
// over, and the 503 that says why nothing could serve it.

// dispatchResult is a successful upstream response plus the provider/model that
// produced it — the caller (handleChat, handleMessages) still owns relaying the
// body back to its own client in its own wire format.
type dispatchResult struct {
	up       *upstream
	provider string
	model    string
}

// dispatchError carries the HTTP status a dispatch failure should surface as,
// since callers speak different error envelopes (OpenAI vs Anthropic shaped).
// retryAfter is how long the caller should wait before routing could succeed
// again; zero when waiting cannot help, and no Retry-After is then advertised.
type dispatchError struct {
	status     int
	msg        string
	retryAfter time.Duration
}

// Error - Returns the message alone. The status travels beside it and is read
// with dispatchStatus, so it never leaks into text a caller might display.
func (e *dispatchError) Error() string { return e.msg }

// dispatchStatus - Returns the HTTP status a dispatch error should be reported
// with, defaulting to 503 for anything that isn't a *dispatchError.
func dispatchStatus(err error) int {
	var de *dispatchError
	if errors.As(err, &de) {
		return de.status
	}
	return http.StatusServiceUnavailable
}

// candidatesFor - Returns the providers that may serve this request and the
// load-balancing strategy to try them in, or the 503 to answer with when the
// set is empty. "chicco:auto" or an unknown model routes across all active
// providers; a known virtual model ID restricts routing to its backends.
//
// Two kinds of request cannot be served by a CLI provider at all, so those are
// dropped before the rotation ever sees them rather than answered wrongly:
// embeddings (a CLI backend returns chat text, not vectors — see runCLI, which
// reads a "messages" key an embeddings payload does not have), and any request
// carrying tool definitions (a CLI backend is handed the conversation as one
// plain-text prompt and narrates the call in prose with finish_reason "stop",
// which an agent cannot tell from a refusal and reports as "no changes made").
func (r *Rotator) candidatesFor(requestedModel string, payload map[string]any, upstreamPath string) ([]Provider, string, error) {
	active, strategy := r.activeForModel(requestedModel)
	if upstreamPath == "/embeddings" {
		active = slices.DeleteFunc(active, func(p Provider) bool { return p.Kind == "cli" })
	}
	wantsTools := false
	if tl, ok := payload["tools"].([]any); ok && len(tl) > 0 {
		wantsTools = true
		active = slices.DeleteFunc(active, func(p Provider) bool { return p.Kind == "cli" })
	}
	if len(active) == 0 {
		msg := "chicco: no providers configured with an API key and models"
		if wantsTools {
			msg = "chicco: request sends 'tools' but every provider for this model is CLI-backed; CLI providers return plain text and cannot emit tool calls"
		}
		// No Retry-After: a config with no usable backend for this request is not
		// something waiting fixes.
		return nil, "", &dispatchError{status: http.StatusServiceUnavailable, msg: msg}
	}
	return active, strategy, nil
}

// blockRejected - Applies one non-2xx upstream reply to the rotation state —
// metrics, the cooldown classifyUpstream chose, and the grey dot for a rejected
// key — and returns the line describing it for the eventual 503 body. It closes
// up.body: the reply is not going to the caller.
func (r *Rotator) blockRejected(p Provider, model string, up *upstream, took time.Duration) string {
	r.metrics.observeError(p.Name, strconv.Itoa(up.status), took)
	snippet, _ := io.ReadAll(io.LimitReader(up.body, cliErrSnippet))
	_ = up.body.Close()
	text := strings.TrimSpace(string(snippet))
	cls := classifyUpstream(up.status, text, up.retryAfter)
	key := p.Name
	if cls.modelScoped {
		key = p.Name + "/" + model // sibling models stay routable
	}
	r.block(key, cls.cooldown, cls.reason)
	if cls.reason == "auth" {
		r.setHealth(p.Name, HealthAuth) // bad key — grey it in the dashboard
	}
	log.Printf("chicco: %s (%s) HTTP %d, blocked %s %s (%s)", p.Name, model, up.status, key, cls.cooldown, cls.reason)
	return fmt.Sprintf("%s: HTTP %d: %s", p.Name, up.status, text)
}

// dispatch - Resolves the active provider set for requestedModel, then walks
// pick() — retrying on transport errors and non-2xx status, applying the same
// cooldown/health/quota bookkeeping handleChat always has — until one provider
// answers with a 2xx or every candidate is exhausted/blocked. It mutates
// payload["model"] in place with each pick before marshaling, so callers must
// pass a payload already shaped for upstreamPath (OpenAI chat-completions for
// "/chat/completions", OpenAI embeddings for "/embeddings"). Shared by
// handleChat, handleMessages and handleEmbeddings so failover/cooldown/quota
// logic lives in exactly one place regardless of which wire format the caller
// used.
func (r *Rotator) dispatch(ctx context.Context, requestedModel string, payload map[string]any, upstreamPath string) (*dispatchResult, error) {
	active, strategy, err := r.candidatesFor(requestedModel, payload, upstreamPath)
	if err != nil {
		return nil, err
	}

	var lastErr string
	for range active {
		p, model, ok := r.pick(active, strategy)
		if !ok {
			break // every provider is in cooldown
		}
		// Override the requested model with the rotation's pick; all other fields
		// the caller sent (temperature, samplers, response_format, stream) pass
		// through untouched.
		payload["model"] = model
		upstreamBody, err := json.Marshal(payload)
		if err != nil {
			return nil, &dispatchError{status: http.StatusInternalServerError, msg: "chicco: re-encode body: " + err.Error()}
		}

		// HTTP providers POST upstream; CLI providers run a subprocess. Both return
		// the same `upstream` so the rest of the loop is identical.
		var up *upstream
		started := time.Now()
		if p.Kind == "cli" {
			up, err = runCLI(ctx, p, model, payload)
		} else {
			up, err = forward(ctx, p, upstreamBody, upstreamPath)
		}
		took := time.Since(started)
		if err != nil {
			r.metrics.observeError(p.Name, "transport", took)
			r.block(p.Name, defaultCooldown, "error")
			lastErr = fmt.Sprintf("%s: %v", p.Name, err)
			log.Printf("chicco: %s (%s) transport error, blocked %s: %v", p.Name, model, defaultCooldown, err)
			continue
		}
		if up.status < 200 || up.status >= 300 {
			lastErr = r.blockRejected(p, model, up, took)
			continue
		}
		log.Printf("chicco: routing to %s (%s)", p.Name, model)
		r.metrics.observeRequest(p.Name, model, took)
		r.setHealth(p.Name, HealthOK) // a 2xx proves the provider works
		return &dispatchResult{up: up, provider: p.Name, model: model}, nil
	}

	var retryAfter time.Duration
	if lastErr == "" {
		// Nothing was even attempted: every candidate was already in cooldown when
		// the request arrived. Saying only "all providers exhausted: " gave no way
		// to tell an exhausted quota from a bad key or a wrong model id, which is
		// exactly the case where the caller most needs to know.
		var summary string
		summary, retryAfter = r.blockedSummary(active)
		lastErr = "nothing attempted — " + summary
	}
	return nil, &dispatchError{
		status:     http.StatusServiceUnavailable,
		msg:        "chicco: all providers exhausted: " + lastErr,
		retryAfter: retryAfter,
	}
}

// blockedSummary - Describes why each candidate was skipped, for the 503 body,
// and returns the shortest cooldown left among them — the earliest instant
// routing could succeed again, which the caller advertises as Retry-After. The
// soonest one, not the longest: one provider coming back is enough to serve the
// request. Both values come out of a single pass under r.mu, since a second
// walk would be reading a block set that may already have moved on.
func (r *Rotator) blockedSummary(active []Provider) (string, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if until, ok := r.blocked[globalKey]; ok && now.Before(until) {
		left := until.Sub(now)
		return fmt.Sprintf("the global quota: is exhausted, resets in %s", left.Round(time.Second)), left
	}
	parts := make([]string, 0, len(active))
	var soonest time.Duration
	note := func(key string, until time.Time) {
		left := until.Sub(now)
		if soonest == 0 || left < soonest {
			soonest = left
		}
		parts = append(parts, fmt.Sprintf("%s: %s, %s left",
			key, cmp.Or(r.reason[key], "blocked"), left.Round(time.Second)))
	}
	for _, p := range active {
		if until, ok := r.blocked[p.Name]; ok && now.Before(until) {
			note(p.Name, until)
			continue
		}
		// Not the provider — the model this virtual model maps to on it.
		for _, m := range p.Models {
			mk := p.Name + "/" + m
			if until, ok := r.blocked[mk]; ok && now.Before(until) {
				note(mk, until)
			}
		}
	}
	if len(parts) == 0 {
		return "no candidate was available", 0
	}
	return strings.Join(parts, "; "), soonest
}

// upstream is one provider's reply, abstracted over HTTP and CLI so handleChat and
// stream treat both the same. body is the (possibly synthesized) SSE/JSON stream.
type upstream struct {
	status      int
	retryAfter  time.Duration
	contentType string
	body        io.ReadCloser
}

// forward - POSTs body to a provider's base URL + path (e.g.
// "/chat/completions", "/embeddings"), carrying its bearer token and
// propagating the caller's context so a client cancel tears down the upstream
// request. The client has no timeout: long streamed generations are bounded by
// the caller's context, not a deadline.
func forward(ctx context.Context, p Provider, body []byte, path string) (*upstream, error) {
	url := strings.TrimRight(p.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // body ownership passes to upstream; stream()/handleChat close it
	if err != nil {
		return nil, err
	}
	return &upstream{
		status:      resp.StatusCode,
		retryAfter:  parseRetryAfter(resp.Header.Get("Retry-After")),
		contentType: resp.Header.Get("Content-Type"),
		body:        resp.Body,
	}, nil
}

// stream - Copies the upstream reply to the client line by line, flushing after
// each chunk so Server-Sent Event deltas arrive promptly, and returns the token
// count reported in the usage field (0 if none). Forwarding is byte-exact —
// ReadBytes keeps the newline — so the proxy stays transparent.
func stream(w http.ResponseWriter, up *upstream) Usage {
	defer up.body.Close()
	if up.contentType != "" {
		w.Header().Set("Content-Type", up.contentType)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// Lines can be large (a whole non-streamed JSON body, or one SSE event), so
	// give the reader a generous buffer.
	br := bufio.NewReaderSize(up.body, 1024*1024)
	var usage Usage
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage
			}
			if flusher != nil {
				flusher.Flush()
			}
			if u := usageSplit(line); u.Total > 0 {
				usage = u // the final usage chunk wins
			}
		}
		if rerr != nil {
			return usage
		}
	}
}

// usageTokens - Extracts usage.total_tokens from one response line — an SSE
// "data: {...}" event or a whole non-streamed JSON body — returning 0 when the
// line carries no usage object.
func usageTokens(line []byte) int64 {
	return usageSplit(line).Total
}

// Usage is the token split a price needs. Input and output are billed at
// different rates everywhere, often by a factor of three or four, so a cost
// computed from the total alone is wrong by whatever the request's shape
// happened to be.
type Usage struct {
	Total      int64
	Prompt     int64
	Completion int64
}

// usageSplit extracts the usage object from one response line — an SSE
// "data: {...}" event or a whole non-streamed JSON body — returning a zero
// Usage when the line carries none.
func usageSplit(line []byte) Usage {
	data := bytes.TrimSpace(line)
	data = bytes.TrimPrefix(data, []byte("data:"))
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return Usage{}
	}
	var env struct {
		Usage struct {
			TotalTokens      int64 `json:"total_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			// Anthropic's own shape, for a provider that answers in it.
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return Usage{}
	}
	u := Usage{
		Total:      env.Usage.TotalTokens,
		Prompt:     env.Usage.PromptTokens,
		Completion: env.Usage.CompletionTokens,
	}
	if u.Prompt == 0 && u.Completion == 0 {
		u.Prompt, u.Completion = env.Usage.InputTokens, env.Usage.OutputTokens
	}
	if u.Total == 0 {
		u.Total = u.Prompt + u.Completion
	}
	return u
}
