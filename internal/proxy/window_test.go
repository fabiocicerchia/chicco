package proxy

import (
	"testing"
	"time"
)

func TestWindowStart(t *testing.T) {
	if !windowStart("none").IsZero() {
		t.Error(`windowStart("none") should be the zero time (never rolls)`)
	}
	d := windowStart("daily")
	if d.Hour() != 0 || d.Minute() != 0 || d.Location() != time.UTC {
		t.Errorf("daily window start = %v, want UTC midnight", d)
	}
	m := windowStart("monthly")
	if m.Day() != 1 || m.Hour() != 0 {
		t.Errorf("monthly window start = %v, want the 1st at 00:00", m)
	}
}

// TestMaybeResetRollsDaily simulates yesterday's counters rolling over to today.
func TestMaybeResetRollsDaily(t *testing.T) {
	r := NewRotator([]Provider{{Name: "g", Window: "daily", Models: []string{"m"}, APIKey: "k"}})
	// Pretend the counters were recorded yesterday.
	r.tokens["g"] = 500
	r.requests["g"] = 9
	r.winStart["g"] = windowStart("daily").AddDate(0, 0, -1)

	r.mu.Lock()
	r.maybeReset("g")
	r.mu.Unlock()

	if r.tokens["g"] != 0 || r.requests["g"] != 0 {
		t.Errorf("counters not reset on day rollover: tokens=%d reqs=%d", r.tokens["g"], r.requests["g"])
	}
	if !r.winStart["g"].Equal(windowStart("daily")) {
		t.Errorf("winStart not advanced to today: %v", r.winStart["g"])
	}
}

// TestMaybeResetKeepsSameDay leaves same-window counters intact.
func TestMaybeResetKeepsSameDay(t *testing.T) {
	r := NewRotator([]Provider{{Name: "g", Window: "daily", Models: []string{"m"}, APIKey: "k"}})
	r.tokens["g"] = 500
	r.winStart["g"] = windowStart("daily") // already today
	r.mu.Lock()
	r.maybeReset("g")
	r.mu.Unlock()
	if r.tokens["g"] != 500 {
		t.Errorf("same-day counters were reset: %d", r.tokens["g"])
	}
}

// TestNoneWindowNeverResets keeps cumulative counters for window: none.
func TestNoneWindowNeverResets(t *testing.T) {
	r := NewRotator([]Provider{{Name: "n", Window: "none", Models: []string{"m"}, APIKey: "k"}})
	r.recordUsage("n", 100)
	r.recordUsage("n", 50)
	if got := r.Snapshot()[0].UsedTokens; got != 150 {
		t.Errorf("none-window tokens = %d, want 150 (no reset)", got)
	}
}
