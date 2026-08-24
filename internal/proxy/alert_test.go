package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func alerterFor(threshold int, webhook string) (*alerter, *recordedPosts) {
	rec := &recordedPosts{}
	a := newAlerter(AlertConfig{ThresholdPercent: threshold, Webhook: webhook})
	a.post = rec.post
	return a, rec
}

type recordedPosts struct {
	mu     sync.Mutex
	bodies [][]byte
	err    error
}

func (r *recordedPosts) post(_ string, body []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, append([]byte(nil), body...))
	return r.err
}

func (r *recordedPosts) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

var day = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func TestFiresOnCrossing(t *testing.T) {
	a, _ := alerterFor(80, "")
	if _, ok := a.check("groq", 799, 1000, "tokens", "daily", day); ok {
		t.Error("79.9% must not fire an 80% threshold")
	}
	al, ok := a.check("groq", 800, 1000, "tokens", "daily", day)
	if !ok {
		t.Fatal("80% did not fire")
	}
	if al.Percent != 80 || al.Used != 800 || al.Quota != 1000 || al.Unit != "tokens" {
		t.Errorf("alert = %+v", al)
	}
}

func TestFiresOncePerCrossingNotPerRequest(t *testing.T) {
	a, _ := alerterFor(80, "")
	if _, ok := a.check("groq", 800, 1000, "tokens", "daily", day); !ok {
		t.Fatal("first crossing did not fire")
	}
	// Every subsequent request in the same window is still over the line.
	for i := 0; i < 50; i++ {
		if _, ok := a.check("groq", 800+int64(i), 1000, "tokens", "daily", day); ok {
			t.Fatalf("fired again at request %d — one crossing, one alert", i)
		}
	}
}

func TestResetsWhenTheWindowRollsOver(t *testing.T) {
	a, _ := alerterFor(80, "")
	if _, ok := a.check("groq", 900, 1000, "tokens", "daily", day); !ok {
		t.Fatal("first day did not fire")
	}
	// Later the same day: still silent.
	if _, ok := a.check("groq", 950, 1000, "tokens", "daily", day.Add(6*time.Hour)); ok {
		t.Error("fired twice in one day")
	}
	// Tomorrow: it must be able to warn again, or the second day is silent.
	if _, ok := a.check("groq", 900, 1000, "tokens", "daily", day.Add(24*time.Hour)); !ok {
		t.Error("did not re-arm after the window rolled")
	}
}

func TestHourlyAndMinutelyWindowsReArmOnTheirOwnBoundary(t *testing.T) {
	a, _ := alerterFor(50, "")
	a.check("p", 60, 100, "requests", "hourly", day)
	if _, ok := a.check("p", 60, 100, "requests", "hourly", day.Add(30*time.Minute)); ok {
		t.Error("hourly fired twice inside one hour")
	}
	if _, ok := a.check("p", 60, 100, "requests", "hourly", day.Add(time.Hour)); !ok {
		t.Error("hourly did not re-arm on the next hour")
	}

	b, _ := alerterFor(50, "")
	b.check("p", 60, 100, "requests", "minutely", day)
	if _, ok := b.check("p", 60, 100, "requests", "minutely", day.Add(61*time.Second)); !ok {
		t.Error("minutely did not re-arm on the next minute")
	}
}

func TestAQuotaThatNeverResetsAlertsOnce(t *testing.T) {
	a, _ := alerterFor(80, "")
	if _, ok := a.check("p", 90, 100, "tokens", "none", day); !ok {
		t.Fatal("did not fire")
	}
	if _, ok := a.check("p", 95, 100, "tokens", "none", day.Add(72*time.Hour)); ok {
		t.Error("a quota with no window must not re-alert three days later")
	}
}

func TestProvidersAreTrackedIndependently(t *testing.T) {
	a, _ := alerterFor(80, "")
	a.check("groq", 900, 1000, "tokens", "daily", day)
	if _, ok := a.check("cerebras", 900, 1000, "tokens", "daily", day); !ok {
		t.Error("one provider's alert suppressed another's")
	}
}

func TestDisabledAndUnquotedProvidersNeverFire(t *testing.T) {
	off, _ := alerterFor(0, "")
	if _, ok := off.check("p", 1000, 1000, "tokens", "daily", day); ok {
		t.Error("threshold 0 must disable alerting")
	}
	on, _ := alerterFor(80, "")
	if _, ok := on.check("p", 1000, 0, "tokens", "daily", day); ok {
		t.Error("a provider with no quota has nothing to be a share of")
	}
}

func TestWebhookPayloadCarriesNoRequestContent(t *testing.T) {
	a, rec := alerterFor(80, "https://example.invalid/hook")
	al, _ := a.check("groq", 900, 1000, "tokens", "daily", day)
	a.fire(al)

	waitFor(t, func() bool { return rec.count() == 1 })

	var got map[string]any
	if err := json.Unmarshal(rec.bodies[0], &got); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	allowed := map[string]bool{
		"provider": true, "threshold_percent": true, "used": true,
		"quota": true, "percent": true, "unit": true, "window": true, "at": true,
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("unexpected field %q in the webhook payload", k)
		}
	}
	if got["provider"] != "groq" {
		t.Errorf("provider = %v", got["provider"])
	}
}

func TestWebhookFailureIsLoggedNotReturned(t *testing.T) {
	a, rec := alerterFor(80, "https://example.invalid/hook")
	rec.err = errors.New("connection refused")
	al, _ := a.check("groq", 900, 1000, "tokens", "daily", day)
	// The point: fire() returns nothing and does not block on the post, so a
	// dead webhook cannot fail a proxied request.
	a.fire(al)
	waitFor(t, func() bool { return rec.count() == 1 })
}

func TestNoWebhookConfiguredStillLogs(t *testing.T) {
	a, rec := alerterFor(80, "")
	al, ok := a.check("groq", 900, 1000, "tokens", "daily", day)
	if !ok {
		t.Fatal("did not fire")
	}
	a.fire(al) // log only
	if rec.count() != 0 {
		t.Error("posted without a webhook configured")
	}
}

func TestCheckBudgetsReadsTheProvidersOwnWindow(t *testing.T) {
	r := NewRotator([]Provider{{
		Name: "groq", APIKey: "k", Models: []string{"m"},
		Quota: Quota{TPD: 1000},
	}}, nil)
	rec := &recordedPosts{}
	r.alerts = newAlerter(AlertConfig{ThresholdPercent: 80, Webhook: "https://example.invalid"})
	r.alerts.post = rec.post

	// Under the line: silent.
	r.recordUsage("groq", "m", 700)
	if rec.count() != 0 {
		t.Fatal("fired below the threshold")
	}
	// Crossing it: one alert, and only one however many requests follow.
	r.recordUsage("groq", "m", 200)
	waitFor(t, func() bool { return rec.count() == 1 })
	r.recordUsage("groq", "m", 50)
	r.recordUsage("groq", "m", 50)
	if rec.count() != 1 {
		t.Errorf("posted %d times, want 1", rec.count())
	}

	var al Alert
	if err := json.Unmarshal(rec.bodies[0], &al); err != nil {
		t.Fatal(err)
	}
	if al.Unit != "tokens" || al.Window != "daily" || al.Quota != 1000 {
		t.Errorf("alert = %+v, want the provider's own TPD window", al)
	}
}

func TestAlertConfigParses(t *testing.T) {
	cfg := loadYAMLAlerts(t, "alerts:\n  threshold_percent: 90\n  webhook: https://example.invalid/h\n")
	if cfg.Alerts.ThresholdPercent != 90 || !strings.HasSuffix(cfg.Alerts.Webhook, "/h") {
		t.Errorf("alerts = %+v", cfg.Alerts)
	}
	// Absent block: off.
	if c := loadYAMLAlerts(t, "addr: 127.0.0.1:41986\n"); c.Alerts.ThresholdPercent != 0 {
		t.Errorf("threshold = %d, want 0 by default", c.Alerts.ThresholdPercent)
	}
}

// waitFor polls a condition rather than sleeping a fixed time: the post runs in
// its own goroutine, which is the property being tested.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// loadYAMLAlerts writes a config and reads it back through LoadConfig, so the
// test covers the path chicco itself takes.
func loadYAMLAlerts(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chicco.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}
