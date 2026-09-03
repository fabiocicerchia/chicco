package proxy

import (
	"math/rand"
	"strings"
	"sync"
	"time"
)

// rotator.go holds the Rotator itself: the providers, the rotation state and
// the counters every other file in this package reads or moves.

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
