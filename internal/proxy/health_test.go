package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeProvider(t *testing.T) {
	srv := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
	}
	ok := srv(200)
	defer ok.Close()
	notFound := srv(404) // provider without a /models endpoint, but key not rejected
	defer notFound.Close()
	unauth := srv(401)
	defer unauth.Close()
	boom := srv(503)
	defer boom.Close()

	cases := []struct {
		base string
		want Health
	}{
		{ok.URL, HealthOK},
		{notFound.URL, HealthOK},
		{unauth.URL, HealthAuth},
		{boom.URL, HealthDown},
		{"http://127.0.0.1:9", HealthDown}, // connection refused
	}
	for _, c := range cases {
		got := probeProvider(context.Background(), Provider{BaseURL: c.base, APIKey: "k"})
		if got != c.want {
			t.Errorf("probeProvider(%s) = %v, want %v", c.base, got, c.want)
		}
	}
}

// TestCheckHealthSetsSnapshot runs the boot probe and checks the snapshot reflects
// each provider's auth state.
func TestCheckHealthSetsSnapshot(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()

	rot := NewRotator([]Provider{
		{Name: "good", BaseURL: good.URL, APIKey: "good", Models: []string{"m"}},
		{Name: "bad", BaseURL: bad.URL, APIKey: "wrong", Models: []string{"m"}},
	})
	rot.CheckHealth(context.Background())

	got := map[string]Health{}
	for _, s := range rot.Snapshot() {
		got[s.Name] = s.Health
	}
	if got["good"] != HealthOK {
		t.Errorf("good health = %v, want HealthOK", got["good"])
	}
	if got["bad"] != HealthAuth {
		t.Errorf("bad health = %v, want HealthAuth", got["bad"])
	}
}

// TestCheckHealthRetriesDown checks a provider that's down on the first probe but
// up on the second ends HealthOK, so a cold-at-boot network self-heals.
func TestCheckHealthRetriesDown(t *testing.T) {
	defer func(d time.Duration) { healthRetryDelay = d }(healthRetryDelay)
	healthRetryDelay = 10 * time.Millisecond

	var hits atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // down on first probe
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer flaky.Close()

	rot := NewRotator([]Provider{{Name: "flaky", BaseURL: flaky.URL, APIKey: "k", Models: []string{"m"}}})
	rot.CheckHealth(context.Background())

	if got := rot.Snapshot()[0].Health; got != HealthOK {
		t.Errorf("flaky health = %v, want HealthOK after retry", got)
	}
}
