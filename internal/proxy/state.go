package proxy

import (
	"encoding/json"
	"os"
	"time"
)

// Token-usage persistence.
//
// ponytail: a JSON state file, not SQLite. The data is a tiny per-provider
// counter map; an atomically-written JSON file persists it across runs/reboots
// with zero dependencies. Reach for SQLite only if this grows into real querying
// or multi-writer concurrency (it isn't there).

// persistedState is the on-disk shape of the usage counters and active cooldowns.
type persistedState struct {
	Tokens      map[string]int64     `json:"tokens"`
	Requests    map[string]int       `json:"requests"`
	WindowStart map[string]time.Time `json:"window_start,omitempty"`
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
	for k, v := range s.Tokens {
		r.tokens[k] = v
	}
	for k, v := range s.Requests {
		r.requests[k] = v
	}
	for k, v := range s.WindowStart {
		r.winStart[k] = v
	}
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
		Tokens:      make(map[string]int64, len(r.tokens)),
		Requests:    make(map[string]int, len(r.requests)),
		WindowStart: make(map[string]time.Time, len(r.winStart)),
		Blocked:     map[string]time.Time{},
		Reason:      map[string]string{},
		Updated:     now,
	}
	for k, v := range r.tokens {
		s.Tokens[k] = v
	}
	for k, v := range r.requests {
		s.Requests[k] = v
	}
	for k, v := range r.winStart {
		s.WindowStart[k] = v
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
