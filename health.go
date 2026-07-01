package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// healthClient bounds each boot probe so a slow or hung provider can't stall
// startup.
var healthClient = &http.Client{Timeout: 5 * time.Second}

// healthRetryDelay is how long a HealthDown probe waits before its single retry,
// so a network that isn't up yet at boot doesn't leave every provider grey.
var healthRetryDelay = 3 * time.Second

// CheckHealth probes every active provider's /v1/models endpoint with its key and
// records the result, so the dashboard can grey out a provider whose token is
// invalid or missing before any real request is routed. It does not consume quota
// (a models listing is free) and updates each provider's dot as its probe returns.
func (r *Rotator) CheckHealth(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range r.Active() {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			h := probeProvider(ctx, p)
			if h == HealthDown { // network may not be up yet at boot — retry once
				select {
				case <-time.After(healthRetryDelay):
					h = probeProvider(ctx, p)
				case <-ctx.Done():
				}
			}
			r.setHealth(p.Name, h)
			switch h {
			case HealthAuth:
				log.Printf("chicco: %s — auth failed (invalid or missing API key)", p.Name)
			case HealthDown:
				log.Printf("chicco: %s — unreachable at boot", p.Name)
			default:
				log.Printf("chicco: %s — healthy", p.Name)
			}
		}(p)
	}
	wg.Wait()
}

// ReprobeLoop re-runs the health check on an interval so a provider that was down
// at boot (network not up yet) or rate-limited (a transient 403) recovers to green
// on its own, without waiting for a real request. Stops when ctx is cancelled.
func (r *Rotator) ReprobeLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.CheckHealth(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// probeProvider reports a provider's liveness. CLI providers are probed by their
// health_command / credential file (see probeCLI). For HTTP providers it does a GET
// on /models with the bearer token: a 401/403 means the key is bad/missing
// (HealthAuth); any other reply means reachable and not rejected (HealthOK,
// covering providers that answer 404); a transport error or 5xx is down (HealthDown).
func probeProvider(ctx context.Context, p Provider) Health {
	if p.Kind == "cli" {
		return probeCLI(ctx, p)
	}
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return HealthDown
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := healthClient.Do(req)
	if err != nil {
		return HealthDown
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return HealthAuth
	case resp.StatusCode >= 500:
		return HealthDown
	default:
		return HealthOK
	}
}

// probeCLI checks a CLI provider without spending quota: run its health_command
// (a local auth-status check), requiring HealthExpect in the output when set —
// this is how a logged-out tool greys (HealthAuth). With no health_command, stat
// the credential file (missing = needs login); otherwise assume healthy.
func probeCLI(ctx context.Context, p Provider) Health {
	if len(p.HealthCommand) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, p.HealthCommand[0], p.HealthCommand[1:]...).CombinedOutput()
		if err != nil {
			// A status command usually exits non-zero when logged out; treat that
			// as auth rather than down so the dot is grey-for-login, not unreachable.
			return HealthAuth
		}
		if p.HealthExpect != "" && !strings.Contains(string(out), p.HealthExpect) {
			return HealthAuth // ran fine but reports "not logged in"
		}
		return HealthOK
	}
	if p.Credential != "" {
		if _, err := os.Stat(p.Credential); err != nil {
			return HealthAuth
		}
	}
	return HealthOK
}
