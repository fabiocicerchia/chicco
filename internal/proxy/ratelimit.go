package proxy

import (
	"time"
)

// eventLog is a compact ring buffer of timestamped request events for one
// provider. Each entry records when the request was made and how many tokens it
// consumed. By scanning the buffer over a sliding window (last 60s, 3600s,
// 86400s) we can count requests and tokens for RPM/RPH/RPD and TPM/TPH/TPD
// without maintaining separate per-window counters.
//
// The buffer is capped at maxEvents. When it is full, the oldest entry is
// overwritten (ring semantics). In practice the daily request limit of any
// free-tier provider is at most a few thousand, so the cap is never tight.
const maxEvents = 10000

type event struct {
	At     time.Time
	Tokens int64
}

type eventLog struct {
	buf  [maxEvents]event
	head int // next write position
	size int // number of valid entries (≤ maxEvents)
}

// record appends a new event to the ring buffer.
func (el *eventLog) record(tokens int64) {
	el.buf[el.head] = event{At: time.Now(), Tokens: tokens}
	el.head = (el.head + 1) % maxEvents
	if el.size < maxEvents {
		el.size++
	}
}

// totals returns the sum of requests and tokens whose timestamp falls within
// the last `window` duration (e.g. time.Minute, time.Hour, 24*time.Hour).
func (el *eventLog) totals(window time.Duration) (reqs int, tokens int64) {
	cutoff := time.Now().Add(-window)
	for i := 0; i < el.size; i++ {
		e := el.buf[i]
		if e.At.After(cutoff) {
			reqs++
			tokens += e.Tokens
		}
	}
	return
}

// dailyTotals returns requests and tokens since UTC midnight today, used for
// the dashboard quota bar's "daily" window (see Provider.effectiveQuota).
func (el *eventLog) dailyTotals() (reqs int, tokens int64) {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < el.size; i++ {
		e := el.buf[i]
		if e.At.After(midnight) {
			reqs++
			tokens += e.Tokens
		}
	}
	return
}

// windowTotals returns the totals for the provider's effective quota window
// (daily / hourly / minutely / none = all-time). "daily" is the only fixed
// clock boundary (UTC midnight); "hourly"/"minutely" are rolling windows.
func (el *eventLog) windowTotals(window string) (reqs int, tokens int64) {
	switch window {
	case "daily":
		return el.dailyTotals()
	case "hourly":
		return el.totals(time.Hour)
	case "minutely":
		return el.totals(time.Minute)
	default:
		// "none" or empty: accumulate forever (match legacy behaviour).
		for i := 0; i < el.size; i++ {
			reqs++
			tokens += el.buf[i].Tokens
		}
		return
	}
}

// check inspects the ring buffer against the given quota and returns the earliest
// time the caller will be unblocked (zero value if no limit is breached). It
// evaluates all six limits (RPM/RPH/RPD/TPM/TPH/TPD) and returns the maximum
// blocked-until time so every active limit is respected.
// Pass the per-model quota when available; fall back to the provider quota otherwise.
func (el *eventLog) check(q Quota) time.Time {
	now := time.Now()
	var blockedUntil time.Time

	// enforce checks one (limit, window) pair. If the rolling-window count of
	// requests or tokens would exceed the limit, it finds the oldest event in
	// that window and sets blockedUntil to when that event exits the window.
	enforce := func(limit int64, window time.Duration, getCount func() int64) {
		if limit <= 0 {
			return
		}
		if getCount() < limit {
			return
		}
		// Find the oldest event still inside the window — the caller unblocks
		// when that event falls out.
		cutoff := now.Add(-window)
		oldest := now // fallback: unblock after the full window
		for i := 0; i < el.size; i++ {
			e := el.buf[i]
			if e.At.After(cutoff) && e.At.Before(oldest) {
				oldest = e.At
			}
		}
		unblockAt := oldest.Add(window)
		if unblockAt.After(blockedUntil) {
			blockedUntil = unblockAt
		}
	}

	rpm, tpm := el.totals(time.Minute)
	rph, tph := el.totals(time.Hour)
	rpd, tpd := el.totals(24 * time.Hour)

	enforce(int64(q.RPM), time.Minute, func() int64 { return int64(rpm) })
	enforce(int64(q.RPH), time.Hour, func() int64 { return int64(rph) })
	enforce(int64(q.RPD), 24*time.Hour, func() int64 { return int64(rpd) })
	enforce(q.TPM, time.Minute, func() int64 { return tpm })
	enforce(q.TPH, time.Hour, func() int64 { return tph })
	enforce(q.TPD, 24*time.Hour, func() int64 { return tpd })

	return blockedUntil
}

// persistedEvents is the on-disk shape for a single provider's event log.
type persistedEvents struct {
	Events []event `json:"events"`
}

// toSlice returns all valid events as a slice (for persistence).
func (el *eventLog) toSlice() []event {
	out := make([]event, el.size)
	for i := 0; i < el.size; i++ {
		out[i] = el.buf[i]
	}
	return out
}

// loadSlice replaces the ring buffer contents with the given slice, ignoring
// entries older than 25 hours (they can never affect any rate-limit window).
func (el *eventLog) loadSlice(events []event) {
	cutoff := time.Now().Add(-25 * time.Hour)
	el.head = 0
	el.size = 0
	for _, e := range events {
		if e.At.Before(cutoff) {
			continue
		}
		el.buf[el.head] = e
		el.head = (el.head + 1) % maxEvents
		if el.size < maxEvents {
			el.size++
		}
	}
}
