package proxy

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parseExposition is a deliberately strict reader of the text format: every
// non-comment line must be `name{labels} value` with a numeric value. It is
// what asserts the output is scrapeable at all, so it accepts nothing a real
// parser would reject.
func parseExposition(t *testing.T, body string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if !strings.HasPrefix(line, "# HELP ") && !strings.HasPrefix(line, "# TYPE ") {
				t.Errorf("comment is neither HELP nor TYPE: %q", line)
			}
			continue
		}
		series, value, ok := strings.Cut(line, " ")
		if !ok {
			t.Errorf("no value on line: %q", line)
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Errorf("value %q on line %q: %v", value, line, err)
			continue
		}
		if strings.Count(series, "{") != strings.Count(series, "}") {
			t.Errorf("unbalanced braces: %q", line)
		}
		out[series] = v
	}
	return out
}

func scrape(t *testing.T, r *Rotator) (string, map[string]float64) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()
	return body, parseExposition(t, body)
}

func TestMetricsCountersMove(t *testing.T) {
	r := NewRotator([]Provider{{Name: "groq", Models: []string{"m"}, APIKey: "k"}}, nil)

	_, before := scrape(t, r)
	if _, ok := before[`chicco_requests_total{provider="groq",model="m"}`]; ok {
		t.Error("a counter exists before anything happened")
	}
	if before["chicco_providers_blocked"] != 0 {
		t.Error("nothing is blocked yet")
	}

	r.metrics.observeRequest("groq", "m", 300*time.Millisecond)
	r.metrics.observeRequest("groq", "m", 2*time.Second)
	r.recordUsage("groq", "m", 1200)
	r.metrics.observeError("groq", "429", 90*time.Millisecond)
	r.block("groq", time.Minute, "limit")

	body, after := scrape(t, r)
	for series, want := range map[string]float64{
		`chicco_requests_total{provider="groq",model="m"}`:             2,
		`chicco_tokens_total{provider="groq",model="m"}`:               1200,
		`chicco_upstream_errors_total{provider="groq",status="429"}`:   1,
		`chicco_provider_blocks_total{provider="groq",reason="limit"}`: 1,
		`chicco_providers_blocked`:                                     1,
	} {
		if got := after[series]; got != want {
			t.Errorf("%s = %v, want %v\n%s", series, got, want, body)
		}
	}

	// Histogram: three observations, cumulative buckets, sum in seconds.
	if got := after[`chicco_upstream_latency_seconds_count{provider="groq"}`]; got != 3 {
		t.Errorf("count = %v, want 3", got)
	}
	// Tolerance, not equality: the sum is accumulated in float64 seconds, so
	// 0.3+2+0.09 lands a few ulps below 2.39.
	if got := after[`chicco_upstream_latency_seconds_sum{provider="groq"}`]; math.Abs(got-2.39) > 1e-9 {
		t.Errorf("sum = %v, want 2.39", got)
	}
	// 0.09 and 0.3 are under 0.5; the 2s one is not.
	if got := after[`chicco_upstream_latency_seconds_bucket{provider="groq",le="0.5"}`]; got != 2 {
		t.Errorf("le=0.5 = %v, want 2", got)
	}
	if got := after[`chicco_upstream_latency_seconds_bucket{provider="groq",le="+Inf"}`]; got != 3 {
		t.Errorf("le=+Inf = %v, want 3", got)
	}
}

func TestMetricsBlockedGaugeFollowsExpiry(t *testing.T) {
	r := NewRotator([]Provider{{Name: "a", Models: []string{"m"}, APIKey: "k"}}, nil)
	r.block("a", -time.Second, "limit") // already elapsed
	if _, m := scrape(t, r); m["chicco_providers_blocked"] != 0 {
		t.Error("an elapsed cooldown still counts as blocked")
	}
	r.block("a", time.Hour, "auth")
	if _, m := scrape(t, r); m["chicco_providers_blocked"] != 1 {
		t.Error("a live cooldown is not counted")
	}
}

func TestMetricsLabelsAreEscaped(t *testing.T) {
	r := NewRotator(nil, nil)
	r.metrics.observeRequest(`we"ird`, "m", time.Second)
	body, _ := scrape(t, r)
	if !strings.Contains(body, `provider="we\"ird"`) {
		t.Errorf("quote in a label was not escaped:\n%s", body)
	}
}

func TestMetricsHandlerRejectsWrites(t *testing.T) {
	r := NewRotator(nil, nil)
	rec := httptest.NewRecorder()
	r.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", rec.Code)
	}
}

// loadYAML writes a config and reads it back through LoadConfig, so the test
// covers the same path chicco itself takes rather than yaml.Unmarshal alone.
func loadYAML(t *testing.T, body string) Config {
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

func TestMetricsConfigDefaultsToItsOwnPort(t *testing.T) {
	cfg := loadYAML(t, "metrics:\n  enabled: true\n")
	if !cfg.Metrics.Enabled || cfg.Metrics.Addr != DefaultMetricsAddr {
		t.Errorf("metrics = %+v, want enabled on %s", cfg.Metrics, DefaultMetricsAddr)
	}

	cfg = loadYAML(t, "metrics:\n  enabled: true\n  addr: \"0.0.0.0:9100\"\n")
	if cfg.Metrics.Addr != "0.0.0.0:9100" {
		t.Errorf("addr = %q", cfg.Metrics.Addr)
	}

	// Absent block: off, and no address invented for it.
	cfg = loadYAML(t, "addr: \"127.0.0.1:41986\"\n")
	if cfg.Metrics.Enabled || cfg.Metrics.Addr != "" {
		t.Errorf("metrics = %+v, want the zero value", cfg.Metrics)
	}
}
