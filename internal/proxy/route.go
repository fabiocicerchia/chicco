package proxy

import (
	"log"
	"time"
)

// route.go decides which provider and model serve a request: the candidate set
// for a virtual model, the order strategies, and the rate-limit-aware pick.

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
	if r.quota != (Quota{}) && r.tripQuota(globalKey, r.quota, now) {
		return Provider{}, "", false
	}

	for _, p := range r.order(active, strategy) {
		if r.inCooldown(p.Name, now) {
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

		if r.tripQuota(p.Name, p.Quota, now) {
			continue
		}

		// Model-scoped block: a per-model quota that tripped on an earlier pick, or
		// an upstream that rejected this specific model (see classifyUpstream).
		// Checked unconditionally — this used to be the else-branch of the per-model
		// quota test below, so any backend declaring a quota silently ignored it.
		if r.inCooldown(mk, now) {
			continue
		}

		// Per-model rate-limit check (only when this backend has its own quota).
		// Blocks this specific model key; NOT the whole provider, so other models
		// on this provider are still reachable.
		if q, hasModelQuota := r.backendQuotas[mk]; hasModelQuota && r.tripQuota(mk, q, now) {
			continue
		}

		return p, model, true
	}
	return Provider{}, "", false
}

// inCooldown - Reports whether key — a provider name, a "provider/model" pair,
// or globalKey — is still blocked at now. The caller must hold r.mu.
func (r *Rotator) inCooldown(key string, now time.Time) bool {
	until, ok := r.blocked[key]
	return ok && now.Before(until)
}

// tripQuota - Checks key's rolling event log against q and, if the window is
// full, puts key in cooldown until the oldest event in that window expires.
// Reports whether it tripped. This is the single copy of the check for the
// global, provider-level and per-model quotas, so a cooldown recorded for one
// of them can't drift from the others. The caller must hold r.mu.
func (r *Rotator) tripQuota(key string, q Quota, now time.Time) bool {
	el, ok := r.eventLogs[key]
	if !ok {
		return false
	}
	until := el.check(q)
	if !until.After(now) {
		return false
	}
	r.blocked[key] = until
	r.reason[key] = "limit"
	r.dirty = true
	return true
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
