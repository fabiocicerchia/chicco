package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultCooldown skips a provider after a transient failure (5xx, network,
	// or a 429 with no Retry-After). authCooldown is longer because a rejected key
	// (401/403) won't fix itself in seconds.
	defaultCooldown = time.Minute
	authCooldown    = time.Hour
)

// Health is a provider's liveness as seen by the boot probe / live requests.
type Health int

const (
	HealthUnknown Health = iota // not yet probed (dashboard shows a "checking" dot)
	HealthOK                    // endpoint answered and the key was accepted
	HealthAuth                  // 401/403 — invalid or missing API key (grey dot)
	HealthDown                  // unreachable or server error at probe time (grey dot)
)

// Rotator holds the upstream providers and the in-process rotation state: a
// shared cursor across providers, a per-provider model cursor, the cooldown-until
// time for providers that recently failed, and per-provider usage counters
// (tokens consumed and requests served) for the dashboard.
type Rotator struct {
	mu        sync.Mutex
	providers []Provider
	modelIdx  map[string]int
	blocked   map[string]time.Time
	tokens    map[string]int64
	requests  map[string]int
	health    map[string]Health
	reason    map[string]string // why a provider is blocked: "limit" | "auth" | "error"
	// winStart[name] is the start of the quota window the current counters belong
	// to; windowOf[name] is the provider's window ("daily"/"monthly"/"none"). When
	// the window rolls over, the counters reset (see maybeReset).
	winStart map[string]time.Time
	windowOf map[string]string
	// statePath, when set, persists tokens/requests across runs (see state.go);
	// dirty marks counters changed since the last write.
	statePath string
	dirty     bool
}

// NewRotator builds a Rotator over the configured providers.
func NewRotator(providers []Provider) *Rotator {
	windowOf := make(map[string]string, len(providers))
	for _, p := range providers {
		windowOf[p.Name] = p.Window
	}
	return &Rotator{
		providers: providers,
		modelIdx:  map[string]int{},
		blocked:   map[string]time.Time{},
		tokens:    map[string]int64{},
		requests:  map[string]int{},
		health:    map[string]Health{},
		reason:    map[string]string{},
		winStart:  map[string]time.Time{},
		windowOf:  windowOf,
	}
}

// windowStart returns the start of the current quota window for a provider, or
// the zero time for "none"/"" (counters never roll over).
func windowStart(window string) time.Time {
	now := time.Now().UTC()
	switch window {
	case "daily":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "monthly":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

// maybeReset zeroes a provider's usage counters when its quota window has rolled
// over since they were last touched. The caller must hold r.mu.
func (r *Rotator) maybeReset(name string) {
	cur := windowStart(r.windowOf[name])
	if !r.winStart[name].Equal(cur) {
		r.tokens[name] = 0
		r.requests[name] = 0
		r.winStart[name] = cur
		r.dirty = true
	}
}

// setHealth records a provider's liveness (from the boot probe or a live request).
func (r *Rotator) setHealth(name string, h Health) {
	r.mu.Lock()
	r.health[name] = h
	r.mu.Unlock()
}

// recordUsage adds a served request (and any reported tokens) to a provider's
// counters. tokens may be 0 when the upstream didn't report usage.
func (r *Rotator) recordUsage(name string, tokens int64) {
	r.mu.Lock()
	r.maybeReset(name)
	r.requests[name]++
	r.tokens[name] += tokens
	r.dirty = true
	r.mu.Unlock()
}

// ProviderStat is a snapshot of one provider's live state for the dashboard.
type ProviderStat struct {
	Name          string
	Models        []string
	QuotaTokens   int64
	QuotaRequests int
	UsedTokens    int64
	Requests      int
	CooldownLeft  time.Duration // 0 when available
	CooldownKind  string        // "limit" | "auth" | "error" when CooldownLeft > 0
	Health        Health
}

// Snapshot returns the current per-provider stats (all configured providers, in
// order) for rendering. Safe to call concurrently with request handling.
func (r *Rotator) Snapshot() []ProviderStat {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]ProviderStat, len(r.providers))
	for i, p := range r.providers {
		r.maybeReset(p.Name) // roll the window before reporting, so idle providers zero out
		var left time.Duration
		var kind string
		if until, ok := r.blocked[p.Name]; ok && now.Before(until) {
			left = until.Sub(now)
			kind = r.reason[p.Name]
		}
		out[i] = ProviderStat{
			Name:          p.Name,
			Models:        p.Models,
			QuotaTokens:   p.QuotaTokens,
			QuotaRequests: p.QuotaRequests,
			UsedTokens:    r.tokens[p.Name],
			Requests:      r.requests[p.Name],
			CooldownLeft:  left,
			CooldownKind:  kind,
			Health:        r.health[p.Name],
		}
	}
	return out
}

// Active returns the providers usable for routing: those with at least one model
// that are authenticated — an HTTP provider needs an api_key; a CLI provider
// authenticates itself (login / credential file), so it needs none.
func (r *Rotator) Active() []Provider {
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		if len(p.Models) == 0 {
			continue
		}
		if p.Kind != "cli" && strings.TrimSpace(p.APIKey) == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// pick returns the first provider not in cooldown, in config order, and its next
// model. Config order is preference order: the top entry is used until it is
// exhausted (rate-limited into cooldown), then the next, so a free tier is drained
// before falling through. ok is false when every active provider is blocked.
func (r *Rotator) pick(active []Provider) (Provider, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for _, p := range active {
		if until, ok := r.blocked[p.Name]; ok && now.Before(until) {
			continue
		}
		i := (r.modelIdx[p.Name] + 1) % len(p.Models)
		r.modelIdx[p.Name] = i
		return p, p.Models[i], true
	}
	return Provider{}, "", false
}

// block puts a provider in cooldown until now+d, recording why ("limit" = usage
// window exhausted, "auth", "error"). The reason drives the dashboard label and is
// persisted so a long window limit survives a restart.
func (r *Rotator) block(name string, d time.Duration, reason string) {
	r.mu.Lock()
	r.blocked[name] = time.Now().Add(d)
	r.reason[name] = reason
	r.dirty = true
	r.mu.Unlock()
}

// isAuth reports whether a status means the key was rejected (401/403), as
// opposed to a rate-limit or transient failure.
func isAuth(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// blockReason maps an upstream status to a cooldown reason for the dashboard.
func blockReason(status int) string {
	switch {
	case isAuth(status):
		return "auth"
	case status == http.StatusTooManyRequests:
		return "limit"
	default:
		return "error"
	}
}

// cooldown picks how long to skip a provider after a failure: a rejected key for
// an hour, an explicit Retry-After when given, otherwise a short default.
func cooldown(status int, retryAfter time.Duration) time.Duration {
	if isAuth(status) {
		return authCooldown
	}
	if retryAfter > 0 {
		return retryAfter
	}
	return defaultCooldown
}

// parseRetryAfter reads a Retry-After header (delta-seconds form) into a
// duration; 0 when absent or not a plain number.
func parseRetryAfter(h string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// Handler returns the HTTP handler: /v1/chat/completions is rotated across
// providers; /health is a liveness probe.
func Handler(r *Rotator) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/chat/completions", r.handleChat)
	return mux
}

// handleChat forwards one chat-completion request, overriding the model with the
// rotation's pick and retrying the next provider on any quota/auth/transient
// failure until one answers (its response is streamed straight back) or all are
// exhausted. The rotation only fails over on the upstream's initial status, which
// is where quota/auth errors surface — once a 2xx body starts streaming to the
// client there is no rewinding it.
func (r *Rotator) handleChat(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "chicco: read body: "+err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "chicco: invalid JSON body: "+err.Error())
		return
	}

	// Ask the upstream to append a final usage chunk so we can count tokens for
	// the dashboard. Harmless to providers that don't support it (extra field),
	// and to the caller (the chunk has empty choices, which clients ignore).
	if s, _ := payload["stream"].(bool); s {
		if _, ok := payload["stream_options"]; !ok {
			payload["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	active := r.Active()
	if len(active) == 0 {
		writeError(w, http.StatusServiceUnavailable, "chicco: no providers configured with an API key and models")
		return
	}

	var lastErr string
	for range active {
		p, model, ok := r.pick(active)
		if !ok {
			break // every provider is in cooldown
		}
		// Override the requested model with the rotation's pick; all other fields
		// the caller sent (temperature, samplers, response_format, stream) pass
		// through untouched.
		payload["model"] = model
		upstreamBody, err := json.Marshal(payload)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chicco: re-encode body: "+err.Error())
			return
		}

		// HTTP providers POST upstream; CLI providers run a subprocess. Both return
		// the same `upstream` so the rest of the loop is identical.
		var up *upstream
		if p.Kind == "cli" {
			// CLI providers return plain text — OpenAI function-calling isn't
			// supported. Warn if the caller sent tool definitions so the gap isn't
			// silent (ciccio never does; it applies its own SEARCH/REPLACE edits).
			if tl, ok := payload["tools"].([]any); ok && len(tl) > 0 {
				log.Printf("chicco: %s is a CLI provider — request 'tools' (function-calling) is ignored; it returns plain text", p.Name)
			}
			up, err = runCLI(req.Context(), p, model, payload)
		} else {
			up, err = forward(req.Context(), p, upstreamBody)
		}
		if err != nil {
			r.block(p.Name, defaultCooldown, "error")
			lastErr = fmt.Sprintf("%s: %v", p.Name, err)
			log.Printf("chicco: %s (%s) transport error, blocked %s: %v", p.Name, model, defaultCooldown, err)
			continue
		}
		if up.status < 200 || up.status >= 300 {
			snippet, _ := io.ReadAll(io.LimitReader(up.body, 512))
			up.body.Close()
			d := cooldown(up.status, up.retryAfter)
			r.block(p.Name, d, blockReason(up.status))
			if isAuth(up.status) {
				r.setHealth(p.Name, HealthAuth) // bad key — grey it in the dashboard
			}
			lastErr = fmt.Sprintf("%s: HTTP %d: %s", p.Name, up.status, strings.TrimSpace(string(snippet)))
			log.Printf("chicco: %s (%s) HTTP %d, blocked %s", p.Name, model, up.status, d)
			continue
		}
		log.Printf("chicco: routing to %s (%s)", p.Name, model)
		r.setHealth(p.Name, HealthOK) // a 2xx proves the provider works
		tokens := stream(w, up)
		r.recordUsage(p.Name, tokens)
		log.Printf("chicco: %s (%s) served %d tokens", p.Name, model, tokens)
		return
	}

	writeError(w, http.StatusServiceUnavailable, "chicco: all providers exhausted: "+lastErr)
}

// upstream is one provider's reply, abstracted over HTTP and CLI so handleChat and
// stream treat both the same. body is the (possibly synthesized) SSE/JSON stream.
type upstream struct {
	status      int
	retryAfter  time.Duration
	contentType string
	body        io.ReadCloser
}

// forward POSTs body to a provider's /chat/completions, carrying its bearer token
// and propagating the caller's context so a client cancel tears down the upstream
// request. The client has no timeout: long streamed generations are bounded by
// the caller's context, not a deadline.
func forward(ctx context.Context, p Provider, body []byte) (*upstream, error) {
	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
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

// stream copies the upstream reply to the client line by line, flushing after each
// chunk so Server-Sent Event deltas arrive promptly, and returns the token count
// reported in the usage field (0 if none). Forwarding is byte-exact — ReadBytes
// keeps the newline — so the proxy stays transparent.
func stream(w http.ResponseWriter, up *upstream) int64 {
	defer up.body.Close()
	if up.contentType != "" {
		w.Header().Set("Content-Type", up.contentType)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// Lines can be large (a whole non-streamed JSON body, or one SSE event), so
	// give the reader a generous buffer.
	br := bufio.NewReaderSize(up.body, 1024*1024)
	var tokens int64
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return tokens
			}
			if flusher != nil {
				flusher.Flush()
			}
			if t := usageTokens(line); t > 0 {
				tokens = t // the final usage chunk wins
			}
		}
		if rerr != nil {
			return tokens
		}
	}
}

// usageTokens extracts usage.total_tokens from one response line — an SSE
// "data: {...}" event or a whole non-streamed JSON body — returning 0 when the
// line carries no usage object.
func usageTokens(line []byte) int64 {
	data := bytes.TrimSpace(line)
	data = bytes.TrimPrefix(data, []byte("data:"))
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return 0
	}
	var env struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return 0
	}
	return env.Usage.TotalTokens
}

// writeError replies with an OpenAI-style error envelope so a client parsing the
// standard shape surfaces a useful message.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "chicco_error"},
	})
}
