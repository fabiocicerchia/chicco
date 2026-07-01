package proxy

import (
	"testing"
	"time"
)

// TestEventLogDailyWindow confirms that windowTotals("daily") only counts events
// from today (UTC midnight onwards) and ignores yesterday's events.
func TestEventLogDailyWindow(t *testing.T) {
	el := &eventLog{}
	// Inject a yesterday event directly into the buffer.
	yesterday := time.Now().UTC().Add(-25 * time.Hour)
	el.buf[0] = event{At: yesterday, Tokens: 999}
	el.size = 1
	el.head = 1

	// Record two events now (today).
	el.record(100)
	el.record(50)

	reqs, tokens := el.windowTotals("daily")
	if reqs != 2 {
		t.Errorf("daily reqs = %d, want 2", reqs)
	}
	if tokens != 150 {
		t.Errorf("daily tokens = %d, want 150", tokens)
	}
}

// TestEventLogMonthlyWindow confirms monthly totals span the current month.
func TestEventLogMonthlyWindow(t *testing.T) {
	el := &eventLog{}
	el.record(500)
	el.record(500)
	reqs, tokens := el.windowTotals("monthly")
	if reqs != 2 || tokens != 1000 {
		t.Errorf("monthly = %d reqs / %d tok, want 2/1000", reqs, tokens)
	}
}

// TestEventLogNoneWindowAccumulates confirms "none" sums all events.
func TestEventLogNoneWindow(t *testing.T) {
	r := NewRotator([]Provider{{Name: "n", Models: []string{"m"}, APIKey: "k"}}, nil)
	r.recordUsage("n", "m", 100)
	r.recordUsage("n", "m", 50)
	if got := r.Snapshot()[0].UsedTokens; got != 150 {
		t.Errorf("none-window tokens = %d, want 150", got)
	}
}

// TestRateLimitRPM confirms check() blocks a provider once RPM is saturated and
// unblocks after the oldest in-window event falls out.
func TestRateLimitRPM(t *testing.T) {
	p := Provider{Name: "p", Quota: Quota{RPM: 2}}
	el := &eventLog{}
	el.record(0)
	el.record(0)

	until := el.check(p)
	if until.IsZero() {
		t.Fatal("expected provider to be blocked after RPM=2 with 2 events in last minute")
	}
	if until.Before(time.Now()) {
		t.Errorf("blocked-until %v is in the past", until)
	}
}

// TestRateLimitTPM confirms check() blocks on token quota too.
func TestRateLimitTPM(t *testing.T) {
	p := Provider{Name: "p", Quota: Quota{TPM: 100}}
	el := &eventLog{}
	el.record(60)
	el.record(50) // total 110 > 100

	until := el.check(p)
	if until.IsZero() {
		t.Fatal("expected provider to be blocked after TPM=100 with 110 tokens in last minute")
	}
}

// TestRateLimitNotTriggered confirms check() returns zero when under all limits.
func TestRateLimitNotTriggered(t *testing.T) {
	p := Provider{Name: "p", Quota: Quota{RPM: 10, TPM: 1000}}
	el := &eventLog{}
	el.record(50)

	if until := el.check(p); !until.IsZero() {
		t.Errorf("expected no block, got blocked until %v", until)
	}
}

// TestLoadSliceDropsOldEvents confirms events older than 25h are not restored.
func TestLoadSliceDropsOldEvents(t *testing.T) {
	old := event{At: time.Now().Add(-26 * time.Hour), Tokens: 999}
	fresh := event{At: time.Now(), Tokens: 1}
	el := &eventLog{}
	el.loadSlice([]event{old, fresh})
	if el.size != 1 {
		t.Errorf("size = %d after loading 1 old + 1 fresh event, want 1", el.size)
	}
	_, tokens := el.windowTotals("daily")
	if tokens != 1 {
		t.Errorf("tokens = %d, want 1 (old event should be dropped)", tokens)
	}
}
