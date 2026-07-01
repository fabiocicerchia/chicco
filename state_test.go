package main

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLimitBlockPersists confirms a usage-limit cooldown is reported with its kind
// and survives a restart (so "limit · resets …" doesn't vanish on relaunch).
func TestLimitBlockPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	r1 := NewRotator([]Provider{{Name: "groq", APIKey: "k", Models: []string{"m"}}})
	r1.EnablePersistence(path)
	r1.block("groq", 2*time.Hour, "limit")
	if s := r1.Snapshot()[0]; s.CooldownKind != "limit" || s.CooldownLeft < time.Hour {
		t.Fatalf("snapshot = %+v, want limit cooldown ~2h", s)
	}
	if err := r1.Persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	r2 := NewRotator([]Provider{{Name: "groq", APIKey: "k", Models: []string{"m"}}})
	r2.EnablePersistence(path)
	if s := r2.Snapshot()[0]; s.CooldownKind != "limit" || s.CooldownLeft < time.Hour {
		t.Errorf("cooldown not restored across restart: %+v", s)
	}
}

// TestPersistenceRoundTrip writes counters, then loads them into a fresh rotator.
func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	r1 := NewRotator([]Provider{{Name: "groq", APIKey: "k", Models: []string{"m"}}})
	r1.EnablePersistence(path) // no file yet — starts empty
	r1.recordUsage("groq", 1500)
	r1.recordUsage("groq", 500)
	if err := r1.Persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	r2 := NewRotator([]Provider{{Name: "groq", APIKey: "k", Models: []string{"m"}}})
	r2.EnablePersistence(path)
	s := r2.Snapshot()
	if len(s) != 1 || s[0].UsedTokens != 2000 || s[0].Requests != 2 {
		t.Errorf("loaded stats = %+v, want 2000 tokens / 2 requests", s)
	}
}

// TestPersistDisabledIsNoop confirms Persist does nothing without a path.
func TestPersistDisabledIsNoop(t *testing.T) {
	r := NewRotator(nil)
	r.recordUsage("x", 10)
	if err := r.Persist(); err != nil {
		t.Errorf("Persist with no state path should be a no-op, got %v", err)
	}
}
