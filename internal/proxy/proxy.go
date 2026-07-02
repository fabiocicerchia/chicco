package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
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
	models    []Model // virtual model routing table from config (may be empty)
	modelIdx  map[string]int
	blocked   map[string]time.Time
	// eventLogs tracks requests and tokens in a ring buffer, keyed by either a
	// provider name (provider-level quota) or a "provider/model" key (per-model
	// quota). pick() records to and checks the most specific key available.
	eventLogs map[string]*eventLog
	// backendQuotas stores the per-model quota for backends that declare one,
	// keyed by "provider/model". Used by pick() to choose between provider-level
	// and model-level rate limit enforcement.
	backendQuotas map[string]Quota
	// modelTokens / modelRequests track usage per "provider/model" key so the
	// dashboard can show a per-model bar alongside the provider total.
	modelTokens   map[string]int64
	modelRequests map[string]int
	health    map[string]Health
	reason    map[string]string // why a provider is blocked: "limit" | "auth" | "error"
	// statePath, when set, persists event logs across runs (see state.go);
	// dirty marks counters changed since the last write.
	statePath string
	dirty     bool
	// authKey, when non-empty, is the shared secret every inbound request (except
	// /health) must present as `Authorization: Bearer <key>`. Empty leaves chicco
	// open (the localhost default). Set once at startup, read-only thereafter.
	authKey string
	// rrCursor advances the round-robin start (shared across virtual models — see
	// order); rng drives the random/weighted orders (used only under r.mu, so it
	// needs no separate lock).
	rrCursor int
	rng      *rand.Rand
}

// NewRotator builds a Rotator over the configured providers and virtual model table.
func NewRotator(providers []Provider, models []Model) *Rotator {
	// Start with one event log per provider (provider-level quota).
	logs := make(map[string]*eventLog, len(providers))
	for _, p := range providers {
		logs[p.Name] = &eventLog{}
	}

	// Build a provider name → Provider map for quick quota lookup below.
	providerMap := make(map[string]Provider, len(providers))
	for _, p := range providers {
		providerMap[p.Name] = p
	}

	// Walk the virtual model table: for every backend that declares its own quota,
	// register a separate "provider/model" event log and store the quota so pick()
	// can enforce it instead of (or in addition to) the provider-level one.
	backendQuotas := make(map[string]Quota)
	for _, m := range models {
		for _, b := range m.Backends {
			if b.Quota == nil {
				continue
			}
			mk := b.Provider + "/" + b.Model
			if _, exists := logs[mk]; !exists {
				logs[mk] = &eventLog{}
			}
			backendQuotas[mk] = *b.Quota
		}
	}

	return &Rotator{
		providers:     providers,
		models:        models,
		modelIdx:      map[string]int{},
		blocked:       map[string]time.Time{},
		eventLogs:     logs,
		backendQuotas: backendQuotas,
		modelTokens:   map[string]int64{},
		modelRequests: map[string]int{},
		health:        map[string]Health{},
		reason:        map[string]string{},
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// setHealth records a provider's liveness (from the boot probe or a live request).
func (r *Rotator) setHealth(name string, h Health) {
	r.mu.Lock()
	r.health[name] = h
	r.mu.Unlock()
}

// recordUsage appends a request event (with token count) to the provider's event
// log and, when a per-model event log exists (backend declared its own quota),
// also records to that. Updates the per-model sub-counters for the dashboard.
// tokens may be 0 when the upstream didn't report usage.
func (r *Rotator) recordUsage(name, model string, tokens int64) {
	r.mu.Lock()
	if el, ok := r.eventLogs[name]; ok {
		el.record(tokens)
	}
	mk := name + "/" + model
	// Also record to the per-model event log when this backend has its own quota.
	if el, ok := r.eventLogs[mk]; ok {
		el.record(tokens)
	}
	r.modelRequests[mk]++
	r.modelTokens[mk] += tokens
	r.dirty = true
	r.mu.Unlock()
}

// ModelStat is the per-model usage snapshot embedded in ProviderStat.
type ModelStat struct {
	Name          string
	Tokens        int64
	Requests      int
	Quota         int64  // per-model quota (0 = use provider quota)
	QuotaIsTokens bool
	QuotaWindow   string
	UsedTokens    int64 // tokens used within the quota window (only set when Quota > 0)
}

// ProviderStat is a snapshot of one provider's live state for the dashboard.
type ProviderStat struct {
	Name          string
	Kind          string // "http" | "cli"
	Models        []ModelStat
	Quota         int64  // effective quota value (0 = no bar); derived from TPD/RPD/TPH/…
	QuotaIsTokens bool   // true → Quota is a token cap; false → request cap
	QuotaWindow   string // "daily" | "hourly" | "minutely" | "none"
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
		var left time.Duration
		var kind string
		if until, ok := r.blocked[p.Name]; ok && now.Before(until) {
			left = until.Sub(now)
			kind = r.reason[p.Name]
		}
		quota, quotaIsTokens, quotaWindow := p.effectiveQuota()
		var usedTokens int64
		var requests int
		if el, ok := r.eventLogs[p.Name]; ok {
			requests, usedTokens = el.windowTotals(quotaWindow)
		}
		models := make([]ModelStat, len(p.Models))
		for j, m := range p.Models {
			mk := p.Name + "/" + m
			ms := ModelStat{
				Name:     m,
				Tokens:   r.modelTokens[mk],
				Requests: r.modelRequests[mk],
			}
			// If this provider/model has a per-model quota, expose its own
			// quota value and window-scoped usage for the dashboard bar.
			if q, ok := r.backendQuotas[mk]; ok {
				mq, mqIsTokens, mqWindow := Backend{Quota: &q}.effectiveQuota(Quota{})
				ms.Quota = mq
				ms.QuotaIsTokens = mqIsTokens
				ms.QuotaWindow = mqWindow
				if el, ok := r.eventLogs[mk]; ok {
					_, ms.UsedTokens = el.windowTotals(mqWindow)
				}
			}
			models[j] = ms
		}
		provKind := p.Kind
		if provKind == "" {
			provKind = "http"
		}
		out[i] = ProviderStat{
			Name:          p.Name,
			Kind:          provKind,
			Models:        models,
			Quota:         quota,
			QuotaIsTokens: quotaIsTokens,
			QuotaWindow:   quotaWindow,
			UsedTokens:    usedTokens,
			Requests:      requests,
			CooldownLeft:  left,
			CooldownKind:  kind,
			Health:        r.health[p.Name],
		}
	}
	return out
}

// VirtualModelIDs returns the IDs of all virtual models defined in the routing
// table, in config order. Used by the /v1/models handler.
func (r *Rotator) VirtualModelIDs() []string {
	ids := make([]string, len(r.models))
	for i, m := range r.models {
		ids[i] = m.ID
	}
	return ids
}

// activeForModel returns the subset of active providers that back a named virtual
// model, each trimmed to only the backend model(s) listed for that virtual model,
// plus the load-balancing strategy configured on that virtual model. For
// "chicco:auto" (or when the requested model doesn't match any virtual model) it
// returns all active providers unchanged and the "order" (config order) strategy,
// since there's no virtual model to carry one.
func (r *Rotator) activeForModel(requested string) (providers []Provider, strategy string) {
	all := r.Active()
	if requested == "chicco:auto" || requested == "" {
		return all, "order"
	}
	// Find the virtual model definition.
	var vm *Model
	for i := range r.models {
		if r.models[i].ID == requested {
			vm = &r.models[i]
			break
		}
	}
	if vm == nil {
		// Unknown model — fall back to full rotation so the caller gets a useful
		// response rather than a 503.
		return all, "order"
	}
	// Build a lookup: provider name → list of backend model names for this VM.
	backendModels := make(map[string][]string, len(vm.Backends))
	for _, b := range vm.Backends {
		if b.Model != "" {
			backendModels[b.Provider] = append(backendModels[b.Provider], b.Model)
		}
	}
	// Keep only active providers that appear in the backend list, replacing their
	// full Models slice with only the backend-specific models so pick() round-robins
	// within the right set.
	out := make([]Provider, 0, len(vm.Backends))
	for _, p := range all {
		if bm, ok := backendModels[p.Name]; ok {
			pc := p
			pc.Models = bm
			out = append(out, pc)
		}
	}
	return out, vm.Strategy
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

// pick returns the first provider not in cooldown — in the order set by the
// requested virtual model's load-balancing strategy ("order", config order, when
// the request doesn't match a virtual model) — and its next model. ok is false
// when every active provider is blocked.
// It also enforces client-side rate limits (RPM/RPH/RPD/TPM/TPH/TPD): if the
// event log shows a limit would be breached, the provider is put in cooldown
// until the oldest event in that window expires.
// When a backend has a per-model quota, that quota is checked against the
// per-model event log (keyed "provider/model") instead of the provider-level one,
// giving each model its own independent rate-limit window.
func (r *Rotator) pick(active []Provider, strategy string) (Provider, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for _, p := range r.order(active, strategy) {
		if until, ok := r.blocked[p.Name]; ok && now.Before(until) {
			continue
		}

		// Advance the model cursor first so we know which model we'd use, then
		// run both the provider-level and (if configured) per-model quota checks.
		i := (r.modelIdx[p.Name] + 1) % len(p.Models)
		model := p.Models[i]
		mk := p.Name + "/" + model

		// Provider-level rate-limit check.
		if el, ok := r.eventLogs[p.Name]; ok {
			if until := el.check(p.Quota); until.After(now) {
				r.blocked[p.Name] = until
				r.reason[p.Name] = "limit"
				r.dirty = true
				continue
			}
		}

		// Per-model rate-limit check (only when this backend has its own quota).
		if q, hasModelQuota := r.backendQuotas[mk]; hasModelQuota {
			if el, ok := r.eventLogs[mk]; ok {
				if until := el.check(q); until.After(now) {
					// Block this specific model key; do NOT block the whole provider
					// so other models on this provider are still reachable.
					r.blocked[mk] = until
					r.reason[mk] = "limit"
					r.dirty = true
					continue
				}
			}
		} else if until, modelBlocked := r.blocked[mk]; modelBlocked && now.Before(until) {
			// Model was previously blocked by a per-model limit — skip it.
			continue
		}

		r.modelIdx[p.Name] = i
		return p, model, true
	}
	return Provider{}, "", false
}

// order returns the active providers in the sequence pick should try them, per the
// requested virtual model's load-balancing strategy. The caller must hold r.mu.
//   - "" / "order":  config order — the top provider is drained (used until it is
//     rate-limited into cooldown), then the request falls through to
//     the next, so a free tier is spent before the fallback. Default.
//   - "round_robin": rotate the starting provider each pick, spreading load evenly
//     instead of always hammering the top entry.
//   - "random":      a fresh random order each pick.
//   - "weighted":    a random order biased by each provider's weight, so a provider
//     with weight 3 leads the order ~3× as often as one with weight 1.
//
// Whatever the order, pick still skips providers in cooldown and handleChat still
// fails over down the list, so a strategy only changes preference, never
// correctness.
func (r *Rotator) order(active []Provider, strategy string) []Provider {
	switch strategy {
	case "round_robin":
		if len(active) == 0 {
			return active
		}
		out := make([]Provider, len(active))
		start := r.rrCursor % len(active)
		r.rrCursor++
		for i := range active {
			out[i] = active[(start+i)%len(active)]
		}
		return out
	case "random":
		out := append([]Provider(nil), active...)
		r.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	case "weighted":
		return r.weightedOrder(active)
	default:
		return active
	}
}

// weightedOrder returns a random permutation of active biased by provider weight:
// it repeatedly draws the next provider with probability proportional to its weight
// among those not yet placed. The caller must hold r.mu.
func (r *Rotator) weightedOrder(active []Provider) []Provider {
	pool := append([]Provider(nil), active...)
	out := make([]Provider, 0, len(pool))
	for len(pool) > 0 {
		total := 0
		for _, p := range pool {
			total += providerWeight(p)
		}
		x := r.rng.Intn(total)
		idx := len(pool) - 1
		for i, p := range pool {
			if x -= providerWeight(p); x < 0 {
				idx = i
				break
			}
		}
		out = append(out, pool[idx])
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}

// providerWeight is a provider's load-balancing weight, defaulting an unset/0 weight
// to 1 so every provider participates.
func providerWeight(p Provider) int {
	if p.Weight > 0 {
		return p.Weight
	}
	return 1
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
// providers; /v1/models lists available virtual models; /health is a liveness
// probe; /v1/status exposes a JSON snapshot for the web dashboard; /dashboard
// serves the live HTML dashboard page. logs may be nil (headless mode) — the
// status endpoint will return an empty log array in that case.
func Handler(r *Rotator, logs *logBuffer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/models", r.handleModels)
	mux.HandleFunc("/v1/chat/completions", r.handleChat)
	mux.HandleFunc("/v1/status", r.handleStatus(logs))
	mux.HandleFunc("/dashboard", handleDashboard)
	return r.withAuth(mux)
}

// withAuth guards every endpoint except /health with the optional shared secret
// (top-level api_key in chicco.yaml). With no key configured chicco stays open, as
// before — fine on 127.0.0.1. Set a key when binding a public addr so only callers
// presenting `Authorization: Bearer <key>` get through. /health is always open so
// liveness probes need no secret.
func (r *Rotator) withAuth(next http.Handler) http.Handler {
	if r.authKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/health" && !bearerMatches(req.Header.Get("Authorization"), r.authKey) {
			writeError(w, http.StatusUnauthorized, "chicco: missing or invalid API key")
			return
		}
		next.ServeHTTP(w, req)
	})
}

// bearerMatches reports whether an Authorization header carries the expected
// bearer token, compared in constant time so a wrong key leaks no timing signal.
func bearerMatches(header, key string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

// handleStatus returns a handler that serves GET /v1/status as JSON containing
// the current provider snapshot and the most recent log lines. It is the data
// source polled by the web dashboard.
func (r *Rotator) handleStatus(logs *logBuffer) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		type modelStatJSON struct {
			Name     string `json:"name"`
			Tokens   int64  `json:"tokens"`
			Requests int    `json:"requests"`
		}
		type providerStatJSON struct {
			Name          string          `json:"name"`
			Kind          string          `json:"kind"`
			Models        []modelStatJSON `json:"models"`
			Quota         int64           `json:"quota"`
			QuotaIsTokens bool            `json:"quota_is_tokens"`
			QuotaWindow   string          `json:"quota_window"`
			UsedTokens    int64           `json:"used_tokens"`
			Requests      int             `json:"requests"`
			CooldownSecs  float64         `json:"cooldown_secs"`
			CooldownKind  string          `json:"cooldown_kind"`
			Health        string          `json:"health"` // "ok" | "auth" | "down" | "unknown"
		}

		healthStr := func(h Health) string {
			switch h {
			case HealthOK:
				return "ok"
			case HealthAuth:
				return "auth"
			case HealthDown:
				return "down"
			default:
				return "unknown"
			}
		}

		stats := r.Snapshot()
		providers := make([]providerStatJSON, len(stats))
		for i, s := range stats {
			ms := make([]modelStatJSON, len(s.Models))
			for j, m := range s.Models {
				ms[j] = modelStatJSON{Name: m.Name, Tokens: m.Tokens, Requests: m.Requests}
			}
			providers[i] = providerStatJSON{
				Name:          s.Name,
				Kind:          s.Kind,
				Models:        ms,
				Quota:         s.Quota,
				QuotaIsTokens: s.QuotaIsTokens,
				QuotaWindow:   s.QuotaWindow,
				UsedTokens:    s.UsedTokens,
				Requests:      s.Requests,
				CooldownSecs:  s.CooldownLeft.Seconds(),
				CooldownKind:  s.CooldownKind,
				Health:        healthStr(s.Health),
			}
		}

		var logLines []string
		if logs != nil {
			logLines = logs.tail(100)
		}
		if logLines == nil {
			logLines = []string{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"providers": providers,
			"logs":      logLines,
		})
	}
}

// handleModels serves GET /v1/models in the OpenAI format: an object list of
// model descriptors. It includes one entry per virtual model defined in the
// routing table plus the catch-all "chicco:auto" that rotates across everything.
func (r *Rotator) handleModels(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ids := r.VirtualModelIDs()
	now := time.Now().Unix()
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelObj, 0, len(ids)+1)
	// chicco:auto first — always present, routes across all active providers.
	data = append(data, modelObj{ID: "chicco:auto", Object: "model", Created: now, OwnedBy: "chicco"})
	for _, id := range ids {
		data = append(data, modelObj{ID: id, Object: "model", Created: now, OwnedBy: "chicco"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
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

	// Determine which subset of providers to route to based on the requested model.
	// "chicco:auto" or an unknown model routes across all active providers.
	// A known virtual model ID restricts routing to its configured backends.
	requestedModel, _ := payload["model"].(string)
	active, strategy := r.activeForModel(requestedModel)
	if len(active) == 0 {
		writeError(w, http.StatusServiceUnavailable, "chicco: no providers configured with an API key and models")
		return
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
			writeError(w, http.StatusInternalServerError, "chicco: re-encode body: "+err.Error())
			return
		}

		// HTTP providers POST upstream; CLI providers run a subprocess. Both return
		// the same `upstream` so the rest of the loop is identical.
		var up *upstream
		if p.Kind == "cli" {
			// CLI providers return plain text — OpenAI function-calling isn't
			// supported. Warn if the caller sent tool definitions so the gap isn't
			// silent (agents that apply their own edits from the text never send them).
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
		r.recordUsage(p.Name, model, tokens)
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
