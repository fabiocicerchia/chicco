package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Warn before a quota runs out, rather than discovering it when requests start
// failing.
//
// A provider in chicco is usually a free tier, so "exhausted" means the
// rotation quietly moves on and the next one drains faster. That is the design
// working, and it is also how a whole pool disappears over an afternoon with
// nothing said until the last one goes. A threshold turns that into a warning
// while there is still headroom.
//
// Three properties this file exists to hold:
//
//   - **Once per crossing.** Firing per request would emit hundreds of lines
//     for one event, which trains everyone to ignore them.
//   - **Reset when the window rolls.** A daily quota that crossed 80% at 4pm
//     must be able to warn again tomorrow, or the second day is silent.
//   - **Never in the way.** A webhook is an outbound call on somebody else's
//     network; it is fired in the background and its failure is logged, never
//     returned. A proxied request must not fail because a notification did.

// AlertConfig is the config block.
type AlertConfig struct {
	// ThresholdPercent fires when usage reaches this share of a provider's
	// quota. 0 disables alerting entirely; 80 is the usual choice.
	ThresholdPercent int `yaml:"threshold_percent"`
	// Webhook, when set, receives a JSON POST per crossing. Optional: the log
	// line is emitted either way.
	Webhook string `yaml:"webhook"`
	// WebhookTimeout bounds that POST. Short by default — this is a
	// notification, not a delivery guarantee.
	WebhookTimeout time.Duration `yaml:"webhook_timeout"`
}

const defaultWebhookTimeout = 5 * time.Second

// Alert is what crossed. Deliberately only these fields: provider, model,
// numbers. No prompt, no completion, no key, nothing derived from a request —
// a webhook is an outbound call to a third party, and the payload is the part
// that would leak if it were pointed at the wrong host.
type Alert struct {
	Provider  string    `json:"provider"`
	Threshold int       `json:"threshold_percent"`
	Used      int64     `json:"used"`
	Quota     int64     `json:"quota"`
	Percent   int       `json:"percent"`
	Unit      string    `json:"unit"`   // "tokens" | "requests"
	Window    string    `json:"window"` // daily | hourly | minutely | none
	At        time.Time `json:"at"`
}

type alerter struct {
	cfg AlertConfig

	mu sync.Mutex
	// fired keys a provider to the window instant it last fired in, so a new
	// window re-arms it without needing a timer to clear anything.
	fired map[string]time.Time

	post func(url string, body []byte) error // injectable for tests
}

func newAlerter(cfg AlertConfig) *alerter {
	if cfg.WebhookTimeout <= 0 {
		cfg.WebhookTimeout = defaultWebhookTimeout
	}
	a := &alerter{cfg: cfg, fired: map[string]time.Time{}}
	a.post = a.httpPost
	return a
}

func (a *alerter) enabled() bool {
	return a != nil && a.cfg.ThresholdPercent > 0
}

// windowStart is the instant the current quota window began. It is what makes
// de-duplication and reset the same mechanism: an alert is remembered against
// its window, and a new window has a different start.
func windowStart(window string, now time.Time) time.Time {
	switch window {
	case "daily":
		// UTC midnight — the boundary the daily limits themselves use.
		return now.UTC().Truncate(24 * time.Hour)
	case "hourly":
		return now.UTC().Truncate(time.Hour)
	case "minutely":
		return now.UTC().Truncate(time.Minute)
	default:
		// No window: one alert for the lifetime of the process, which is the
		// only honest reading of a quota that never resets.
		return time.Time{}
	}
}

// check evaluates one provider's usage and returns the Alert if this crossing
// has not already been reported for the current window.
func (a *alerter) check(provider string, used, quota int64, unit, window string, now time.Time) (Alert, bool) {
	if !a.enabled() || quota <= 0 {
		return Alert{}, false
	}
	pct := int(used * 100 / quota)
	if pct < a.cfg.ThresholdPercent {
		return Alert{}, false
	}

	start := windowStart(window, now)
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.fired[provider]; ok && last.Equal(start) {
		return Alert{}, false // already said so for this window
	}
	a.fired[provider] = start
	return Alert{
		Provider: provider, Threshold: a.cfg.ThresholdPercent,
		Used: used, Quota: quota, Percent: pct,
		Unit: unit, Window: window, At: now,
	}, true
}

// fire logs the alert and, when configured, posts it. The post runs in its own
// goroutine: the caller is on the request path.
func (a *alerter) fire(al Alert) {
	log.Printf("chicco: %s at %d%% of its %s quota (%d/%d %s) — threshold %d%%",
		al.Provider, al.Percent, al.Window, al.Used, al.Quota, al.Unit, al.Threshold)
	if a.cfg.Webhook == "" {
		return
	}
	body, err := json.Marshal(al)
	if err != nil {
		log.Printf("chicco: could not encode budget alert: %v", err)
		return
	}
	go func() {
		if err := a.post(a.cfg.Webhook, body); err != nil {
			// Logged, never returned: a notification that failed must not fail
			// somebody's completion.
			log.Printf("chicco: budget alert webhook failed: %v", err)
		}
	}()
}

func (a *alerter) httpPost(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.WebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// checkBudgets evaluates every provider against its quota after a request has
// been recorded. Called from the request path, so it does no I/O itself — fire
// logs synchronously (cheap) and posts in the background.
func (r *Rotator) checkBudgets(now time.Time) {
	if !r.alerts.enabled() {
		return
	}
	type pending struct{ al Alert }
	var out []pending

	r.mu.Lock()
	for _, p := range r.providers {
		quota, isTokens, window := p.effectiveQuota()
		if quota <= 0 {
			continue
		}
		el, ok := r.eventLogs[p.Name]
		if !ok {
			continue
		}
		reqs, tokens := el.windowTotals(window)
		used := int64(reqs)
		unit := "requests"
		if isTokens {
			used, unit = tokens, "tokens"
		}
		if al, fire := r.alerts.check(p.Name, used, quota, unit, window, now); fire {
			out = append(out, pending{al})
		}
	}
	r.mu.Unlock()

	for _, p := range out {
		r.alerts.fire(p.al)
	}
}
