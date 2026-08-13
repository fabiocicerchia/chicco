package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// healthClient bounds each boot probe so a slow or hung provider can't stall
// startup. 15s, not 5s: the first probe after boot pays for a COLD DNS cache, and
// 5s was short enough that a first lookup which merely took its time was recorded
// as "unreachable". A probe that is only there to colour a dot must not report a
// provider dead because it was impatient.
var healthClient = &http.Client{Timeout: 15 * time.Second}

// healthRetryDelay is how long a HealthDown probe waits before its single retry,
// so a network that isn't up yet at boot doesn't leave every provider grey.
var healthRetryDelay = 3 * time.Second

// probeSlots bounds how many provider probes run at once.
//
// The measured failure was NOT cpu: with the error text finally logged, a cold-cache
// boot reported six providers unreachable with two causes, and neither was load —
//
//	dial tcp [2606:4700:7::2c3]:443: connect: network is unreachable
//	lookup openrouter.ai on 127.0.0.53:53: read udp ...: i/o timeout
//
// — an AAAA record on a host with no IPv6 route, and systemd-resolved's stub
// dropping UDP queries when eleven arrive at once. Only the first run of five
// failed; the rest hit a warm cache, which is why the "down" set looked random.
//
// Bounding concurrency is still the right lever, because what overwhelms the stub
// resolver is the number of simultaneous lookups. Sized from the CPU count as a
// proportional-to-the-machine heuristic rather than a magic constant, floored at 2
// so a single-core box still overlaps two probes, and capped at 6 because the
// constraint being respected is a local resolver, not the CPU.
func probeSlots() int {
	return min(max(runtime.NumCPU()/2, 2), 6)
}

// CheckHealth probes every active provider's /v1/models endpoint with its key and
// records the result, so the dashboard can grey out a provider whose token is
// invalid or missing before any real request is routed. It does not consume quota
// (a models listing is free) and updates each provider's dot as its probe returns.
func (r *Rotator) CheckHealth(ctx context.Context) {
	var wg sync.WaitGroup
	// Buffered channel as a counting semaphore. Applies to every probe: HTTP ones
	// because concurrent DNS lookups are what the stub resolver chokes on, CLI ones
	// because each forks a Node process.
	slots := make(chan struct{}, probeSlots())
	// probe runs one attempt while holding a slot. The slot is released before the
	// retry delay below, so a waiting provider isn't blocked by another's backoff.
	probe := func(p Provider) (Health, error) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return HealthUnknown, ctx.Err()
		}
		return probeProvider(ctx, p)
	}
	for _, p := range r.Active() {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			h, err := probe(p)
			if h == HealthDown { // network may not be up yet at boot — retry once
				select {
				case <-time.After(healthRetryDelay):
					h, err = probe(p)
				case <-ctx.Done():
				}
			}
			r.setHealth(p.Name, h)
			switch h {
			case HealthAuth:
				log.Printf("chicco: %s — auth failed (invalid or missing API key)", p.Name)
			case HealthDown:
				// Report WHY. "unreachable at boot" on its own is unfalsifiable: it
				// cannot be told apart from a timeout under load, a DNS failure or a
				// genuinely dead endpoint — and this probe is wrong often enough that
				// the difference is the whole story.
				log.Printf("chicco: %s — unreachable at boot: %v", p.Name, err)
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

// probeProvider reports a provider's liveness, and why when it is HealthDown. CLI
// providers are probed by their health_command / credential file (see probeCLI).
// For HTTP providers it does a GET on /models with the bearer token: a 401/403
// means the key is bad/missing (HealthAuth); any other reply means reachable and
// not rejected (HealthOK, covering providers that answer 404); a transport error or
// 5xx is down (HealthDown).
func probeProvider(ctx context.Context, p Provider) (Health, error) {
	if p.Kind == "cli" {
		return probeCLI(ctx, p)
	}
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return HealthDown, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := healthClient.Do(req)
	if err != nil {
		return HealthDown, err
	}
	defer resp.Body.Close()
	switch {
	case isAuth(resp.StatusCode):
		return HealthAuth, nil
	case resp.StatusCode >= 500:
		return HealthDown, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	default:
		return HealthOK, nil
	}
}

// probeCLI checks a CLI provider without spending quota: run its health_command
// (a local auth-status check), requiring HealthExpect in the output when set —
// this is how a logged-out tool greys (HealthAuth). With no health_command, stat
// the credential file (missing = needs login); otherwise assume healthy.
func probeCLI(ctx context.Context, p Provider) (Health, error) {
	// The tool has to exist before anything else is worth asking. A provider
	// configured for a CLI that isn't installed here (common in the container
	// image, which ships none of them) otherwise probed green — its credential
	// file mounted from the host, or no health_command at all — and only failed
	// at request time, with the binary's own "no such file or directory" as the
	// caller's 502.
	if p.Command != "" {
		if _, err := exec.LookPath(p.Command); err != nil {
			return HealthDown, err
		}
	}
	if len(p.HealthCommand) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, p.HealthCommand[0], p.HealthCommand[1:]...).CombinedOutput()
		if err != nil {
			// A status command usually exits non-zero when logged out; treat that
			// as auth rather than down so the dot is grey-for-login, not unreachable.
			return HealthAuth, err
		}
		if p.HealthExpect != "" && !strings.Contains(string(out), p.HealthExpect) {
			return HealthAuth, nil // ran fine but reports "not logged in"
		}
		return HealthOK, nil
	}
	if p.Credential != "" {
		if _, err := os.Stat(p.Credential); err != nil {
			return HealthAuth, err
		}
	}
	return HealthOK, nil
}
