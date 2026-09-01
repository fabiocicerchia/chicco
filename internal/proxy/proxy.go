package proxy

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"slices"
	"sort"
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
	// requestErrorCooldown is used for request-shaped 4xx (400/404/413/422): the
	// payload, not the provider, was rejected, so a healthy provider must not be
	// locked out for future (differently-sized) requests. It's just long enough to
	// exclude the provider from the *current* request's failover loop.
	// ponytail: 5s covers a normal fast-failing loop; a provider that takes >5s to
	// 4xx could be re-tried once within the same request — harmless, the loop is
	// bounded by the provider count.
	requestErrorCooldown = 5 * time.Second
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
	health        map[string]Health
	reason        map[string]string // why a provider is blocked: "limit" | "auth" | "error"
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
	// aliases map a caller-facing name onto a virtual model id. Read under
	// r.mu so a SIGHUP reload can change them without a restart.
	aliases map[string]string
	// metrics holds the Prometheus counters. Always present so the record hooks
	// need no nil check on the hot path; only exposed when a metrics listener is
	// configured (see Run).
	metrics *metrics
	// costs prices requests from the config's price list. Always non-nil; it
	// reports every model as unpriced until prices are configured.
	costs *costTracker
	// alerts warns before a quota runs out. Always non-nil; disabled unless a
	// threshold is configured.
	alerts *alerter
	// quota is the optional top-level cap from Config.Quota, applied across every
	// provider combined via the eventLogs[globalKey] log (see pick). Zero Quota{}
	// (the default) means no aggregate cap.
	quota Quota
}

// globalKey is the sentinel eventLogs/blocked/reason key for the optional
// top-level quota (Config.Quota), which caps usage across every provider
// combined rather than one. It can't collide with a real key: provider names
// come from YAML identifiers (no "/"), and per-model keys always contain "/".
const globalKey = "__global__"

// NewRotator - Builds a Rotator over the configured providers and virtual model
// table.
func NewRotator(providers []Provider, models []Model) *Rotator {
	// Start with one event log per provider (provider-level quota), plus one
	// sentinel log accumulating every request across all providers combined,
	// for the optional global quota (see globalKey).
	logs := make(map[string]*eventLog, len(providers)+1)
	for _, p := range providers {
		logs[p.Name] = &eventLog{}
	}
	logs[globalKey] = &eventLog{}

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
		aliases:       map[string]string{},
		metrics:       newMetrics(),
		costs:         newCostTracker(Pricing{}),
		alerts:        newAlerter(AlertConfig{}),
		// Weighted provider pick and tie shuffle only — nothing here is a secret,
		// a token or a nonce, so math/rand is the right tool. //nolint:gosec
		rng: rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // load balancing, not security
	}
}

// setHealth - Records a provider's liveness (from the boot probe or a live
// request).
func (r *Rotator) setHealth(name string, h Health) {
	r.mu.Lock()
	r.health[name] = h
	r.mu.Unlock()
}

// recordUsage - Appends a request event (with token count) to the provider's
// event log and, when a per-model event log exists (backend declared its own
// quota), also records to that. Updates the per-model sub-counters for the
// dashboard. tokens may be 0 when the upstream didn't report usage.
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
	r.eventLogs[globalKey].record(tokens) // always present (NewRotator/Reload)
	r.modelRequests[mk]++
	r.modelTokens[mk] += tokens
	r.dirty = true
	r.mu.Unlock()
	r.metrics.observeTokens(name, model, tokens)
	// After the counters move, not before: the check reads the same totals the
	// rate limiter does, so it must see this request.
	r.checkBudgets(time.Now())
}

// ModelStat is the per-model usage snapshot embedded in ProviderStat.
type ModelStat struct {
	Name          string
	Tokens        int64
	Requests      int
	Quota         int64 // per-model quota (0 = use provider quota)
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
	// Inactive is true when the provider is missing an api_key or has no
	// models configured (see Provider.isActive) — it will never be probed or
	// routed to, as distinct from Health == HealthUnknown, which just means a
	// probe hasn't returned yet.
	Inactive bool
}

// Snapshot - Returns the current per-provider stats (all configured providers,
// in order) for rendering. Safe to call concurrently with request handling.
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
			Inactive:      !p.isActive(),
		}
	}
	return out
}

// DailyTotals - Sums every active provider's dailyTotals() (since UTC midnight)
// for the dashboard's aggregate usage line. It uses each provider's own
// eventLog directly rather than Snapshot()'s per-quota-window totals, since
// providers may use different quota windows (daily/hourly/minutely/none) —
// summing those directly would be apples-to-oranges. A consistent "since UTC
// midnight" basis is used for every provider regardless of its own configured
// quota window.
func (r *Rotator) DailyTotals() (requests int, tokens int64, activeProviders int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.providers {
		if !p.isActive() {
			continue
		}
		activeProviders++
		if el, ok := r.eventLogs[p.Name]; ok {
			req, tok := el.dailyTotals()
			requests += req
			tokens += tok
		}
	}
	return
}

// VirtualModelIDs - Returns the IDs of the virtual models a request could
// actually be served by, in config order. A model every one of whose backends
// is greyed out — key rejected, tool logged out, CLI binary not installed,
// endpoint unreachable — is left out: listing it only gets a caller to pick it
// and take a 502/503, since nothing in the listing said it was dead. Cooldown
// is NOT a reason to hide a model: that's a rate limit which reopens on its
// own, and the entry would flap in and out of the list.
//
// Used by the /v1/models handler.
func (r *Rotator) VirtualModelIDs() []string {
	ids := make([]string, 0, len(r.models))
	for _, m := range r.models {
		if r.servable(m.ID) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// servable - Reports whether any provider backing a virtual model is currently
// in a state that could answer. A provider never probed (HealthUnknown) counts
// as servable — unproven is not the same as broken — and a model with no
// candidate providers at all is left alone: that's a config gap, not a probe
// result, and hiding it would just make a mistyped backend name look like an
// outage.
func (r *Rotator) servable(id string) bool {
	providers, _ := r.activeForModel(id)
	if len(providers) == 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range providers {
		if h := r.health[p.Name]; h != HealthDown && h != HealthAuth {
			return true
		}
	}
	return false
}

// resolveAlias maps a caller-facing name onto the virtual model it stands for,
// returning the input unchanged when it is not an alias. Validated at load, so
// a name reaching here always points at a model that exists.
func (r *Rotator) resolveAlias(requested string) (string, bool) {
	r.mu.Lock()
	target, ok := r.aliases[requested]
	r.mu.Unlock()
	if !ok {
		return requested, false
	}
	return target, true
}

// Aliases is a copy of the alias table, for the status endpoint and the UI.
func (r *Rotator) Aliases() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.aliases))
	for k, v := range r.aliases {
		out[k] = v
	}
	return out
}

// activeForModel - Returns the subset of active providers that back a named
// virtual model, each trimmed to only the backend model(s) listed for that
// virtual model, plus the load-balancing strategy configured on that virtual
// model. For "chicco:auto" (or when the requested model doesn't match any
// virtual model) it returns all active providers unchanged and the "order"
// (config order) strategy, since there's no virtual model to carry one.
func (r *Rotator) activeForModel(requested string) (providers []Provider, strategy string) {
	if target, aliased := r.resolveAlias(requested); aliased {
		// Logged because a request routed somewhere other than where it asked
		// is exactly what someone reading these logs is trying to follow.
		log.Printf("chicco: alias %s -> %s", requested, target)
		requested = target
	}
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
	// Build lookups keyed by provider name: the backend model names for this VM,
	// and the backend entry itself (for its optional weight override — see
	// Backend.effectiveWeight).
	backendModels := make(map[string][]string, len(vm.Backends))
	backendOf := make(map[string]Backend, len(vm.Backends))
	for _, b := range vm.Backends {
		if b.Model != "" {
			backendModels[b.Provider] = append(backendModels[b.Provider], b.Model)
		}
		backendOf[b.Provider] = b
	}
	// Keep only active providers that appear in the backend list, replacing their
	// full Models slice with only the backend-specific models so pick() round-robins
	// within the right set, and applying this model's weight override, if any.
	out := make([]Provider, 0, len(vm.Backends))
	for _, p := range all {
		if bm, ok := backendModels[p.Name]; ok {
			pc := p
			pc.Models = bm
			pc.Weight = backendOf[p.Name].effectiveWeight(p.Weight)
			out = append(out, pc)
		}
	}
	return out, vm.Strategy
}

// isActive - Reports whether p is usable for routing: it needs at least one
// model, and — unless it's a CLI provider, which authenticates itself (login /
// credential file) — a non-empty api_key.
func (p Provider) isActive() bool {
	if len(p.Models) == 0 {
		return false
	}
	return p.Kind == "cli" || strings.TrimSpace(p.APIKey) != ""
}

// Active - Returns the providers usable for routing (see Provider.isActive). It
// locks r.mu because Reload can swap r.providers concurrently with a live
// request.
func (r *Rotator) Active() []Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		if p.isActive() {
			out = append(out, p)
		}
	}
	return out
}

// pick - Returns the first provider not in cooldown — in the order set by the
// requested virtual model's load-balancing strategy ("order", config order,
// when the request doesn't match a virtual model) — and its next model. ok is
// false when every active provider is blocked. It also enforces client-side
// rate limits (RPM/RPH/RPD/TPM/TPH/TPD): if the event log shows a limit would
// be breached, the provider is put in cooldown until the oldest event in that
// window expires. When a backend has a per-model quota, that quota is checked
// against the per-model event log (keyed "provider/model") instead of the
// provider-level one, giving each model its own independent rate-limit window.
func (r *Rotator) pick(active []Provider, strategy string) (Provider, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()

	// Global cap check (optional top-level quota:) — once per pick(), not once
	// per provider, since it isn't provider-specific. Trips exactly like a
	// per-provider block: the caller's existing "every provider is in cooldown"
	// path (a 503) covers it with no new error handling.
	if r.quota != (Quota{}) {
		if until := r.eventLogs[globalKey].check(r.quota); until.After(now) {
			r.blocked[globalKey] = until
			r.reason[globalKey] = "limit"
			r.dirty = true
			return Provider{}, "", false
		}
	}

	for _, p := range r.order(active, strategy) {
		if until, ok := r.blocked[p.Name]; ok && now.Before(until) {
			continue
		}

		// Advance the model cursor first so we know which model we'd use, then
		// run both the provider-level and (if configured) per-model quota checks.
		// The cursor is committed here, BEFORE the checks below, so a model skipped
		// by one of them isn't re-chosen by the next pick() in the same failover
		// loop — otherwise one blocked model made the whole provider unreachable
		// for the rest of the request.
		i := (r.modelIdx[p.Name] + 1) % len(p.Models)
		model := p.Models[i]
		mk := p.Name + "/" + model
		r.modelIdx[p.Name] = i

		// Provider-level rate-limit check.
		if el, ok := r.eventLogs[p.Name]; ok {
			if until := el.check(p.Quota); until.After(now) {
				r.blocked[p.Name] = until
				r.reason[p.Name] = "limit"
				r.dirty = true
				continue
			}
		}

		// Model-scoped block: a per-model quota that tripped on an earlier pick, or
		// an upstream that rejected this specific model (see classifyUpstream).
		// Checked unconditionally — this used to be the else-branch of the per-model
		// quota test below, so any backend declaring a quota silently ignored it.
		if until, ok := r.blocked[mk]; ok && now.Before(until) {
			continue
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
		}

		return p, model, true
	}
	return Provider{}, "", false
}

// order - Returns the active providers in the sequence pick should try them,
// per the requested virtual model's load-balancing strategy. The caller must
// hold r.mu.
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

// weightedOrder - Returns a random permutation of active biased by provider
// weight: it repeatedly draws the next provider with probability proportional
// to its weight among those not yet placed. The caller must hold r.mu.
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

// providerWeight - Is a provider's load-balancing weight, defaulting an unset/0
// weight to 1 so every provider participates.
func providerWeight(p Provider) int {
	if p.Weight > 0 {
		return p.Weight
	}
	return 1
}

// block - Puts a provider in cooldown until now+d, recording why ("limit" =
// usage window exhausted, "auth", "error"). The reason drives the dashboard
// label and is persisted so a long window limit survives a restart.
func (r *Rotator) block(name string, d time.Duration, reason string) {
	r.mu.Lock()
	r.blocked[name] = time.Now().Add(d)
	r.reason[name] = reason
	r.dirty = true
	r.mu.Unlock()
	// Outside the lock: the metrics mutex is a different one, and taking it
	// under r.mu is the only way this package could deadlock.
	r.metrics.observeBlock(name, reason)
}

// isAuth - Reports whether a status means the key was rejected (401/403), as
// opposed to a rate-limit or transient failure.
func isAuth(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// blockReason - Maps an upstream status to a cooldown reason for the dashboard.
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

// isRequestError - Reports whether a status means the request itself was
// rejected (bad body, unknown model, payload too large) rather than the
// provider being unhealthy or rate-limited. Waiting won't help these, and they
// say nothing about whether the next request would succeed — so they get only a
// brief cooldown.
func isRequestError(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// cooldown - Picks how long to skip a provider after a failure: a rejected key
// for an hour, a brief skip for a request-shaped 4xx (the payload was at fault,
// not the provider), an explicit Retry-After when given, otherwise a short
// default.
func cooldown(status int, retryAfter time.Duration) time.Duration {
	if isAuth(status) {
		return authCooldown
	}
	if isRequestError(status) {
		return requestErrorCooldown
	}
	if retryAfter > 0 {
		return retryAfter
	}
	return defaultCooldown
}

// quotaBodyRe / modelBodyRe read an upstream's error TEXT, because the status
// alone is ambiguous: 403 is used for a rejected key, for "free quota
// exhausted" (Alibaba) and for "model is not available on this plan"
// (Cloudflare). Treating all three as an auth failure cooled the whole provider
// for an hour and greyed it as "invalid key", which took every OTHER model on
// that provider down with it — one wrong model id in the config was enough to
// lose five working ones.
var (
	quotaBodyRe = regexp.MustCompile(`(?i)(quota|credit|billing|insufficient|exhaust|payment|spend|top ?up|upgrade)`)
	// `.` not `[^.]`: model ids contain dots (@cf/moonshotai/kimi-k2.7-code), which
	// is exactly the id that slipped through and cost the provider an hour.
	modelBodyRe = regexp.MustCompile(`(?i)model.{0,100}?(not available|not found|does not exist|unsupported|invalid|no access|not supported)` +
		`|(?i)(invalid|unknown|unsupported).{0,20}model`)
)

// upstreamClass is how one non-2xx upstream reply should be handled: how long to
// skip, what to call it on the dashboard, and whether the skip applies to the
// whole provider or only the model that was asked for.
type upstreamClass struct {
	reason      string // "auth" | "limit" | "error"
	cooldown    time.Duration
	modelScoped bool // block "provider/model", leaving sibling models routable
}

// classifyUpstream - Decides that, from the status, the Retry-After header and
// the error body. Body text is only ever used to make a verdict LESS severe
// than the status alone would imply, so a provider that says nothing useful
// behaves exactly as before.
func classifyUpstream(status int, body string, retryAfter time.Duration) upstreamClass {
	// Quota wording is checked FIRST and stays provider-wide: an exhausted account
	// affects every model on it, and such messages often mention "model" too
	// ("...to keep using the model, upgrade"), which must not scope it to one model.
	switch {
	case status == http.StatusPaymentRequired: // 402 — out of credits
		return upstreamClass{"limit", cmp.Or(retryAfter, authCooldown), false}
	case status == http.StatusForbidden && quotaBodyRe.MatchString(body):
		// "free quota has been exhausted" — a real limit, not a bad key. Labelled
		// honestly so the dashboard stops claiming the API key is invalid.
		return upstreamClass{"limit", cmp.Or(retryAfter, authCooldown), false}
	}
	// A complaint about the named model is about the request, not the provider:
	// skip just this model, briefly, and leave its siblings routable.
	if modelBodyRe.MatchString(body) && (isAuth(status) || isRequestError(status)) {
		return upstreamClass{"error", requestErrorCooldown, true}
	}
	return upstreamClass{blockReason(status), cooldown(status, retryAfter), false}
}

// parseRetryAfter - Reads a Retry-After header (delta-seconds form) into a
// duration; 0 when absent or not a plain number.
func parseRetryAfter(h string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// Handler - Returns the HTTP handler: /v1/chat/completions is rotated across
// providers; /v1/messages is the same rotation in Anthropic's wire format;
// /v1/models lists available virtual models; /health is a liveness probe;
// /v1/status exposes a JSON snapshot for the web dashboard; /dashboard serves
// the live HTML dashboard page. logs may be nil (e.g. a caller with no use for
// log history) — the status endpoint returns an empty log array then.
func Handler(r *Rotator, logs *logBuffer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", r.handleHealth)
	mux.HandleFunc("/v1/models", r.handleModels)
	mux.HandleFunc("/v1/chat/completions", r.handleChat)
	mux.HandleFunc("/v1/embeddings", r.handleEmbeddings)
	mux.HandleFunc("/v1/messages", r.handleMessages)
	mux.HandleFunc("/v1/status", r.handleStatus(logs))
	mux.HandleFunc("/dashboard", handleDashboard)
	return r.withAuth(mux)
}

// withAuth - Guards every endpoint except /health with the optional shared
// secret (top-level api_key in chicco.yaml). With no key configured chicco
// stays open, as before — fine on 127.0.0.1. Set a key when binding a public
// addr so only callers presenting `Authorization: Bearer <key>` get through.
// /health is always open so liveness probes need no secret. The key is read per
// request (under r.mu) so a SIGHUP reload can add, change, or remove it without
// a restart.
//
// A browser cannot set an Authorization header by typing a URL, so /dashboard
// also accepts the key once as ?key=<secret> and hands back a cookie; the page
// and its /v1/status polling then authenticate with that cookie.
func (r *Rotator) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := r.currentAuthKey()
		if key != "" && req.URL.Path != "/health" && !authorized(req, key) {
			if req.URL.Path == "/dashboard" && secretMatches(req.URL.Query().Get("key"), key) {
				http.SetCookie(w, &http.Cookie{
					Name:     authCookie,
					Value:    key,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
				// Redirect so the secret stops riding in the address bar.
				http.Redirect(w, req, "/dashboard", http.StatusFound)
				return
			}
			writeError(w, http.StatusUnauthorized, "chicco: missing or invalid API key")
			return
		}
		next.ServeHTTP(w, req)
	})
}

// authCookie is the browser-side carrier for the inbound shared secret, set by
// a /dashboard?key=<secret> visit.
const authCookie = "chicco_key"

// authorized - Reports whether a request presents the shared secret, either as
// a bearer token (API clients) or as the dashboard cookie (browsers).
func authorized(req *http.Request, key string) bool {
	if bearerMatches(req.Header.Get("Authorization"), key) {
		return true
	}
	c, err := req.Cookie(authCookie)
	return err == nil && secretMatches(c.Value, key)
}

// currentAuthKey - Returns the inbound shared secret under r.mu, so a reload
// writing it doesn't race the auth check reading it.
func (r *Rotator) currentAuthKey() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authKey
}

// bearerMatches - Reports whether an Authorization header carries the expected
// bearer token, compared in constant time so a wrong key leaks no timing
// signal.
func bearerMatches(header, key string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return secretMatches(strings.TrimSpace(header[len(prefix):]), key)
}

// secretMatches - Constant-time equality for the inbound shared secret, so a
// wrong key leaks no timing signal whichever way it was presented.
func secretMatches(got, key string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

// healthStr - Renders a Health for the JSON APIs (/health, /v1/status) and the
// dashboard, which colour on these four strings.
func healthStr(h Health) string {
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

// handleHealth - Serves GET /health. The status stays 200 whatever the
// providers are doing — the Helm chart points both the liveness AND the
// readiness probe here, so failing it on a provider outage would restart a
// proxy that is running perfectly well. The body is what changed: an empty 200
// said nothing about whether anything could actually serve a request, so a
// provider whose CLI isn't installed or whose key is rejected looked exactly as
// healthy as a working one. "degraded" means every provider is greyed out or in
// cooldown right now.
func (r *Rotator) handleHealth(w http.ResponseWriter, _ *http.Request) {
	providers := map[string]string{}
	ready := 0
	for _, s := range r.Snapshot() {
		if s.Inactive {
			continue
		}
		state := healthStr(s.Health)
		if s.CooldownLeft > 0 {
			state = "cooldown"
		}
		providers[s.Name] = state
		if state == "ok" || state == "unknown" { // unknown = not probed yet, still routable
			ready++
		}
	}
	status := "ok"
	if ready == 0 {
		status = "degraded"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "providers": providers})
}

// handleStatus - Returns a handler that serves GET /v1/status as JSON
// containing the current provider snapshot and the most recent log lines. It is
// the data source polled by the web dashboard.
func (r *Rotator) handleStatus(logs *logBuffer) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		type modelStatJSON struct {
			Name          string `json:"name"`
			Tokens        int64  `json:"tokens"`
			Requests      int    `json:"requests"`
			Quota         int64  `json:"quota"` // per-model quota (0 = use the provider's)
			QuotaIsTokens bool   `json:"quota_is_tokens"`
			UsedTokens    int64  `json:"used_tokens"` // only meaningful when Quota > 0
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
			Inactive      bool            `json:"inactive"`
		}

		stats := r.Snapshot()
		providers := make([]providerStatJSON, len(stats))
		for i, s := range stats {
			ms := make([]modelStatJSON, len(s.Models))
			for j, m := range s.Models {
				ms[j] = modelStatJSON{
					Name:          m.Name,
					Tokens:        m.Tokens,
					Requests:      m.Requests,
					Quota:         m.Quota,
					QuotaIsTokens: m.QuotaIsTokens,
					UsedTokens:    m.UsedTokens,
				}
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
				Inactive:      s.Inactive,
			}
		}

		var logLines []string
		if logs != nil {
			logLines = logs.tail(100)
		}
		if logLines == nil {
			logLines = []string{}
		}

		reqToday, tokToday, activeN := r.DailyTotals()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"providers":        providers,
			"logs":             logLines,
			"requests_today":   reqToday,
			"tokens_today":     tokToday,
			"active_providers": activeN,
			// Session spend. Carries its own currency label and its unpriced
			// counts, so a dashboard cannot render the total as the whole bill
			// when part of the traffic could not be priced.
			"cost": r.CostSummary(),
		})
	}
}

// handleModels - Serves GET /v1/models in the OpenAI format: an object list of
// model descriptors. It includes one entry per virtual model defined in the
// routing table plus the catch-all "chicco:auto" that rotates across
// everything.
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
	// Aliases are listed too: a caller that cannot see them has to be told out
	// of band what names exist, which is the coupling aliases exist to remove.
	aliases := r.Aliases()
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data = append(data, modelObj{ID: name, Object: "model", Created: now, OwnedBy: "chicco (alias for " + aliases[name] + ")"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// handleChat - Forwards one chat-completion request, overriding the model with
// the rotation's pick and retrying the next provider on any
// quota/auth/transient failure until one answers (its response is streamed
// straight back) or all are exhausted. The rotation only fails over on the
// upstream's initial status, which is where quota/auth errors surface — once a
// 2xx body starts streaming to the client there is no rewinding it.
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

	requestedModel, _ := payload["model"].(string)
	res, err := r.dispatch(req.Context(), requestedModel, payload, "/chat/completions")
	if err != nil {
		setRetryAfter(w, err)
		writeError(w, dispatchStatus(err), err.Error())
		return
	}
	usage := stream(w, res.up)
	r.recordUsage(res.provider, res.model, usage.Total)
	log.Printf("chicco: %s (%s) served %d tokens%s",
		res.provider, res.model, usage.Total, r.costNote(res.provider, res.model, usage))
}

// handleEmbeddings - Forwards one embeddings request the same way handleChat
// forwards a chat completion — rotation, cooldown and quota bookkeeping all go
// through the shared dispatch(). Embeddings responses are a single JSON object,
// never streamed, so unlike handleChat this reads the upstream body fully and
// relays it verbatim.
func (r *Rotator) handleEmbeddings(w http.ResponseWriter, req *http.Request) {
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

	requestedModel, _ := payload["model"].(string)
	res, err := r.dispatch(req.Context(), requestedModel, payload, "/embeddings")
	if err != nil {
		setRetryAfter(w, err)
		writeError(w, dispatchStatus(err), err.Error())
		return
	}
	defer res.up.body.Close()
	respBody, err := io.ReadAll(res.up.body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "chicco: read upstream response: "+err.Error())
		return
	}
	var parsed struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	r.recordUsage(res.provider, res.model, parsed.Usage.TotalTokens)
	log.Printf("chicco: %s (%s) served embeddings, %d tokens%s", res.provider, res.model,
		parsed.Usage.TotalTokens,
		r.costNote(res.provider, res.model, Usage{Total: parsed.Usage.TotalTokens, Prompt: parsed.Usage.TotalTokens}))

	contentType := res.up.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(res.up.status)
	_, _ = w.Write(respBody)
}

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
	// Determine which subset of providers to route to based on the requested model.
	// "chicco:auto" or an unknown model routes across all active providers.
	// A known virtual model ID restricts routing to its configured backends.
	active, strategy := r.activeForModel(requestedModel)
	// CLI providers return plain chat text, not vectors — routing an embeddings
	// request to one yields a 2xx body that isn't an embedding. Drop them here so
	// they can't be picked (see runCLI, which reads "messages" an embeddings
	// payload doesn't have).
	if upstreamPath == "/embeddings" {
		active = slices.DeleteFunc(active, func(p Provider) bool { return p.Kind == "cli" })
	}
	// Same for function-calling: a CLI provider is handed the conversation as one
	// plain-text prompt and answers with plain text, so a request carrying tool
	// definitions comes back with the call narrated in prose (often pseudo-XML)
	// and finish_reason "stop" — indistinguishable from a refusal to any agent,
	// which then reports "no changes made". Drop them rather than serve that.
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
		return nil, &dispatchError{status: http.StatusServiceUnavailable, msg: msg}
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
			lastErr = fmt.Sprintf("%s: HTTP %d: %s", p.Name, up.status, text)
			log.Printf("chicco: %s (%s) HTTP %d, blocked %s %s (%s)", p.Name, model, up.status, key, cls.cooldown, cls.reason)
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

// setRetryAfter - Advertises when routing could next succeed, for a dispatch
// failure that carries a cooldown. Both the OpenAI and Anthropic SDKs honour
// Retry-After natively, so without it a client backs off on its own schedule
// and spends its whole retry budget inside a cooldown that outlasts it. Called
// on the exhaustion path in either wire format (writeError and
// writeAnthropicError both write w.Header() before the status line), never on
// the 4xx paths, where an unchanged request fails again however long you wait.
// RFC 9110 wants whole delta-seconds; round up and never emit 0, which reads as
// "retry now" and turns a back-off into a hot loop.
func setRetryAfter(w http.ResponseWriter, err error) {
	var de *dispatchError
	if !errors.As(err, &de) || de.retryAfter <= 0 {
		return
	}
	secs := (de.retryAfter + time.Second - 1) / time.Second
	w.Header().Set("Retry-After", strconv.FormatInt(int64(secs), 10))
}

// writeError - Replies with an OpenAI-style error envelope so a client parsing
// the standard shape surfaces a useful message.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "chicco_error"},
	})
}
