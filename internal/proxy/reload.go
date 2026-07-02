package proxy

import (
	"context"
	"log"
	"strings"
)

// Config hot-reload.
//
// A running chicco can pick up chicco.yaml edits (a rotated key, an added or removed
// provider, a new strategy) without a restart — which would otherwise drop the
// dashboard and in-flight state. On SIGHUP (see watchSIGHUP, non-Windows) the config
// is re-read, validated, and applied via Reload, which keeps the live usage
// counters and cooldowns of providers that survive the edit.

// Reload swaps in a new provider set and virtual model table (each model may carry
// its own load-balancing strategy) and inbound api_key from a re-read config
// without dropping live state: usage counters,
// cooldowns and health are keyed by provider name (or "provider/model" for a
// per-model quota), so a provider or backend still present keeps its
// tokens/requests/cooldown across the reload; a removed one's state is dropped and
// a new one starts fresh. The listen address is not changed — the socket is already
// bound, so an addr edit needs a real restart.
func (r *Rotator) Reload(cfg Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = cfg.Providers
	r.models = cfg.Models
	r.authKey = cfg.APIKey
	r.quota = cfg.Quota

	keep := make(map[string]bool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		keep[p.Name] = true
		if _, ok := r.eventLogs[p.Name]; !ok {
			r.eventLogs[p.Name] = &eventLog{}
		}
	}

	backendQuotas := make(map[string]Quota)
	keepKey := make(map[string]bool, len(keep)+1)
	for k := range keep {
		keepKey[k] = true
	}
	// The global-quota sentinel log/cooldown isn't tied to any provider — keep it
	// across every reload, or a SIGHUP would silently drop the running total and
	// any active global cooldown.
	keepKey[globalKey] = true
	if _, ok := r.eventLogs[globalKey]; !ok {
		r.eventLogs[globalKey] = &eventLog{}
	}
	for _, m := range cfg.Models {
		for _, b := range m.Backends {
			if b.Quota == nil {
				continue
			}
			mk := b.Provider + "/" + b.Model
			keepKey[mk] = true
			if _, ok := r.eventLogs[mk]; !ok {
				r.eventLogs[mk] = &eventLog{}
			}
			backendQuotas[mk] = *b.Quota
		}
	}
	r.backendQuotas = backendQuotas

	// Forget state for providers/backends no longer in the config, so their
	// counters don't linger in the state file or a future snapshot.
	forget(r.eventLogs, keepKey)
	forget(r.modelTokens, keepKey)
	forget(r.modelRequests, keepKey)
	forget(r.blocked, keepKey)
	forget(r.reason, keepKey)
	forget(r.health, keep)
	forget(r.modelIdx, keep)
}

// forget deletes every entry of m whose key is not in keep.
func forget[V any](m map[string]V, keep map[string]bool) {
	for k := range m {
		if !keep[k] {
			delete(m, k)
		}
	}
}

// reloadFromFile re-reads the config file and applies it to the rotator, preserving
// live state. A parse error or a hard validation problem aborts the reload — the
// running config is kept — while warnings (inactive providers, unknown regions) are
// logged. On success it re-probes health so added providers get a dot. Returns
// whether the reload was applied.
func reloadFromFile(rot *Rotator, path string) bool {
	cfg, err := LoadConfig(path)
	if err != nil {
		log.Printf("chicco: reload failed, keeping current config: %v", err)
		return false
	}
	var hard []string
	for _, p := range cfg.Validate() {
		if strings.HasPrefix(p, "warning:") {
			log.Printf("chicco: reload %s", p)
		} else {
			hard = append(hard, p)
		}
	}
	if len(hard) > 0 {
		for _, p := range hard {
			log.Printf("chicco: reload rejected — %s", p)
		}
		return false
	}
	rot.Reload(cfg)
	go rot.CheckHealth(context.Background())
	log.Printf("chicco: config reloaded from %s (%d provider(s))", path, len(cfg.Providers))
	return true
}
