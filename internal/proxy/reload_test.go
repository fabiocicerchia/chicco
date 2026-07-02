package proxy

import (
	"testing"
	"time"
)

// TestReloadPreservesLiveState confirms a reload keeps the usage counters and
// cooldown of a provider that survives the edit, applies its new fields, adds a new
// provider, and updates the virtual model table (and its per-model strategy) /
// inbound key.
func TestReloadPreservesLiveState(t *testing.T) {
	r := NewRotator([]Provider{{Name: "a", APIKey: "k", Models: []string{"m"}}}, nil)
	r.recordUsage("a", "m", 100)
	r.block("a", time.Hour, "limit")

	r.Reload(Config{
		APIKey: "secret",
		Providers: []Provider{
			{Name: "a", APIKey: "k", Models: []string{"m"}, Quota: Quota{TPD: 500}},
			{Name: "b", APIKey: "k", Models: []string{"m2"}},
		},
		Models: []Model{{ID: "v", Strategy: "random", Backends: []Backend{{Provider: "a", Model: "m"}}}},
	})

	if _, strategy := r.activeForModel("v"); strategy != "random" {
		t.Errorf("model v strategy = %q, want random", strategy)
	}
	if r.currentAuthKey() != "secret" {
		t.Errorf("authKey = %q, want secret", r.currentAuthKey())
	}

	byName := map[string]ProviderStat{}
	for _, s := range r.Snapshot() {
		byName[s.Name] = s
	}
	if len(byName) != 2 {
		t.Fatalf("snapshot has %d providers, want 2 (a, b)", len(byName))
	}
	if a := byName["a"]; a.UsedTokens != 100 || a.CooldownLeft <= 0 || a.Quota != 500 {
		t.Errorf("provider a = %+v, want tokens=100, live cooldown, new quota=500", a)
	}
	if _, ok := byName["b"]; !ok {
		t.Errorf("new provider b missing after reload")
	}
}

// TestReloadPreservesGlobalQuota confirms a reload picks up an edited top-level
// quota and keeps the global event log's history (and, by the same map-keyed
// mechanism, a live global cooldown) instead of resetting it — the same
// treatment every per-provider log already gets across a SIGHUP.
func TestReloadPreservesGlobalQuota(t *testing.T) {
	r := NewRotator([]Provider{{Name: "a", APIKey: "k", Models: []string{"m"}}}, nil)
	r.quota = Quota{RPD: 5}
	r.recordUsage("a", "m", 10)
	r.recordUsage("a", "m", 10)

	r.Reload(Config{
		Providers: []Provider{{Name: "a", APIKey: "k", Models: []string{"m"}}},
		Quota:     Quota{RPD: 3},
	})

	if r.quota != (Quota{RPD: 3}) {
		t.Errorf("quota after reload = %+v, want {RPD:3}", r.quota)
	}
	r.mu.Lock()
	el, ok := r.eventLogs[globalKey]
	r.mu.Unlock()
	if !ok {
		t.Fatal("global event log dropped on reload")
	}
	if reqs, _ := el.dailyTotals(); reqs != 2 {
		t.Errorf("global log requests after reload = %d, want 2 (history preserved)", reqs)
	}
}

// TestReloadDropsRemovedProvider confirms a provider removed from the config loses
// its state and disappears from the snapshot.
func TestReloadDropsRemovedProvider(t *testing.T) {
	r := NewRotator([]Provider{
		{Name: "a", APIKey: "k", Models: []string{"m"}},
		{Name: "gone", APIKey: "k", Models: []string{"m"}},
	}, nil)
	r.recordUsage("gone", "m", 50)

	r.Reload(Config{Providers: []Provider{{Name: "a", APIKey: "k", Models: []string{"m"}}}})

	r.mu.Lock()
	_, present := r.eventLogs["gone"]
	r.mu.Unlock()
	if present {
		t.Errorf("removed provider's counters were not cleared")
	}
	if s := r.Snapshot(); len(s) != 1 || s[0].Name != "a" {
		t.Errorf("snapshot = %+v, want only [a]", s)
	}
}
