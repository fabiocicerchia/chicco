package proxy

import "testing"

// orderNames runs one order() pass under the lock and returns the provider names.
func orderNames(r *Rotator, active []Provider, strategy string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.order(active, strategy)
	names := make([]string, len(out))
	for i, p := range out {
		names[i] = p.Name
	}
	return names
}

// TestStrategyDefaultIsConfigOrder confirms the default strategy preserves config
// order (drain the top provider first).
func TestStrategyDefaultIsConfigOrder(t *testing.T) {
	ps := []Provider{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	r := NewRotator(ps, nil)
	got := orderNames(r, ps, "") // strategy ""
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("default order = %v, want [a b c]", got)
	}
}

// TestStrategyRoundRobin confirms the starting provider rotates each pick.
func TestStrategyRoundRobin(t *testing.T) {
	ps := []Provider{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	r := NewRotator(ps, nil)
	var firsts []string
	for i := 0; i < 6; i++ {
		o := orderNames(r, ps, "round_robin")
		if len(o) != 3 {
			t.Fatalf("round_robin dropped providers: %v", o)
		}
		firsts = append(firsts, o[0])
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if firsts[i] != want[i] {
			t.Fatalf("round_robin firsts = %v, want %v", firsts, want)
		}
	}
}

// TestStrategyRandomIsPermutation confirms random keeps every provider and, over
// many draws, leads with each of them at least once.
func TestStrategyRandomIsPermutation(t *testing.T) {
	ps := []Provider{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	r := NewRotator(ps, nil)
	seenFirst := map[string]bool{}
	for i := 0; i < 300; i++ {
		o := orderNames(r, ps, "random")
		if len(o) != 3 {
			t.Fatalf("random dropped providers: %v", o)
		}
		seenFirst[o[0]] = true
	}
	if len(seenFirst) != 3 {
		t.Errorf("random never led with some provider: %v", seenFirst)
	}
}

// TestStrategyWeighted confirms a heavier provider leads the order far more often,
// roughly in proportion to its weight.
func TestStrategyWeighted(t *testing.T) {
	ps := []Provider{{Name: "heavy", Weight: 9}, {Name: "light", Weight: 1}}
	r := NewRotator(ps, nil)
	const n = 3000
	heavyFirst := 0
	for i := 0; i < n; i++ {
		if orderNames(r, ps, "weighted")[0] == "heavy" {
			heavyFirst++
		}
	}
	// Expected ~90%; use a wide band to keep the test robust against variance.
	if heavyFirst < n*3/4 {
		t.Errorf("weighted: heavy led %d/%d (%.0f%%), want ~90%%", heavyFirst, n, 100*float64(heavyFirst)/n)
	}
}

// TestStrategyPerModel confirms two virtual models can use different strategies
// independently — one round_robin, one plain config order — routed by
// activeForModel, without a global strategy setting.
func TestStrategyPerModel(t *testing.T) {
	ps := []Provider{
		{Name: "a", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}},
		{Name: "b", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}},
	}
	models := []Model{
		{ID: "rr", Strategy: "round_robin", Backends: []Backend{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}}},
		{ID: "ordered", Strategy: "", Backends: []Backend{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}}},
	}
	r := NewRotator(ps, models)

	active, strategy := r.activeForModel("rr")
	if strategy != "round_robin" {
		t.Errorf("rr model strategy = %q, want round_robin", strategy)
	}
	var firsts []string
	for i := 0; i < 4; i++ {
		firsts = append(firsts, orderNames(r, active, strategy)[0])
	}
	if firsts[0] == firsts[1] && firsts[1] == firsts[2] && firsts[2] == firsts[3] {
		t.Errorf("round_robin model firsts never rotated: %v", firsts)
	}

	_, strategy2 := r.activeForModel("ordered")
	if strategy2 != "" {
		t.Errorf("ordered model strategy = %q, want \"\"", strategy2)
	}

	_, autoStrategy := r.activeForModel("chicco:auto")
	if autoStrategy != "order" {
		t.Errorf("chicco:auto strategy = %q, want order", autoStrategy)
	}
}
