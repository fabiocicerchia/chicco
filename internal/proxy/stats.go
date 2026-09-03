package proxy

import (
	"time"
)

// stats.go turns the Rotator's live state into the snapshot the dashboards
// render — nothing here decides anything, it only reports.

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
