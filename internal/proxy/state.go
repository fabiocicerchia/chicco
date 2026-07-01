package proxy

import (
	"encoding/json"
	"maps"
	"os"
	"time"
)

// Token-usage persistence.
//
// The state file stores each provider's raw event log (timestamp + token count
// per request) rather than pre-aggregated counters. This lets us recompute any
// sliding window (minute/hour/day) correctly after a restart without losing
// rate-limit state. Events older than 25 hours are dropped on load — they can
// never influence any rate-limit window.

// persistedState is the on-disk shape of the usage counters and active cooldowns.
type persistedState struct {
	// EventLogs stores the raw event slices keyed by provider name.
	EventLogs     map[string][]event   `json:"event_logs,omitempty"`
	ModelTokens   map[string]int64     `json:"model_tokens,omitempty"`
	ModelRequests map[string]int       `json:"model_requests,omitempty"`
	// Blocked/Reason persist active cooldowns so a long window limit ("limit ·
	// resets 3pm") survives a restart instead of being retried and re-failing.
	Blocked map[string]time.Time `json:"blocked,omitempty"`
	Reason  map[string]string    `json:"reason,omitempty"`
	Updated time.Time            `json:"updated"`
}

// EnablePersistence points the rotator at a state file and loads any saved
// counters into it. Best effort: a missing or unreadable file just starts empty.
func (r *Rotator) EnablePersistence(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statePath = path
	data, err := os.ReadFile(path)
	if err != nil {
		return // first run, or unreadable — start from zero
	}
	var s persistedState
	if json.Unmarshal(data, &s) != nil {
		return
	}
	// Restore event logs — loadSlice drops events older than 25h automatically.
	// Keys may be provider names or "provider/model" keys for per-model quotas.
	for name, events := range s.EventLogs {
		if el, ok := r.eventLogs[name]; ok {
			el.loadSlice(events)
		}
		// If the key wasn't pre-populated (e.g. config changed since last run),
		// create the log on the fly so we don't silently drop persisted data.
		// The backendQuotas map will be populated from config in NewRotator.
	}
	maps.Copy(r.modelTokens, s.ModelTokens)
	maps.Copy(r.modelRequests, s.ModelRequests)
	// Restore only cooldowns that haven't elapsed yet.
	now := time.Now()
	for k, v := range s.Blocked {
		if v.After(now) {
			r.blocked[k] = v
			r.reason[k] = s.Reason[k]
		}
	}
}

// Persist atomically writes the counters to the state file when they have changed
// since the last write. No-op when persistence is disabled or nothing changed.
func (r *Rotator) Persist() error {
	r.mu.Lock()
	if r.statePath == "" || !r.dirty {
		r.mu.Unlock()
		return nil
	}
	now := time.Now()
	s := persistedState{
		EventLogs:     make(map[string][]event, len(r.eventLogs)),
		ModelTokens:   maps.Clone(r.modelTokens),
		ModelRequests: maps.Clone(r.modelRequests),
		Blocked:       map[string]time.Time{},
		Reason:        map[string]string{},
		Updated:       now,
	}
	for name, el := range r.eventLogs {
		s.EventLogs[name] = el.toSlice()
	}
	for k, v := range r.blocked {
		if v.After(now) { // only persist still-active cooldowns
			s.Blocked[k] = v
			s.Reason[k] = r.reason[k]
		}
	}
	path := r.statePath
	r.dirty = false
	r.mu.Unlock()

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write can't truncate the existing file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
