// Package proxy implements chicco's local OpenAI-compatible rotation proxy: it
// serves one /v1/chat/completions endpoint and forwards each request to the next
// free-tier provider in the config, round-robining models and skipping providers
// that hit a quota/rate-limit (429) or auth (401/403) error until one answers.
package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// Options configures a chicco run. All fields come from cmd/chicco flags.
type Options struct {
	ConfigPath string
	Addr       string // overrides the config's addr when non-empty
	StatePath  string // token-usage state file; empty disables persistence
	Headless   bool   // no dashboard; log plainly to stderr
	Version    string // build version, shown in log lines
}

// Run loads the config, starts the health probes and persistence loop, and
// serves until the dashboard exits (or forever, headless). It returns an error
// for fatal startup problems; the caller owns the process exit code.
func Run(opts Options) error {
	cfg, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		return err
	}
	if opts.Addr != "" {
		cfg.Addr = opts.Addr
	}

	rot := NewRotator(cfg.Providers, cfg.Models)
	rot.authKey = cfg.APIKey // shared secret guarding inbound requests, if set
	active := rot.Active()
	if len(active) == 0 {
		return fmt.Errorf("no providers with an API key and models are configured —\n"+
			"        check the `providers:` list and the api_key env vars in %s", opts.ConfigPath)
	}

	names := make([]string, len(active))
	for i, p := range active {
		names[i] = p.Name
	}

	// Probe every provider at boot (auth + liveness, no quota spent) so dead keys
	// show up grey immediately, then re-probe periodically so a provider that was
	// down at boot or transiently rate-limited recovers to green on its own. Runs
	// in the background — dots start "checking" and flip as probes return.
	go rot.CheckHealth(context.Background())
	go rot.ReprobeLoop(context.Background(), 5*time.Minute)

	// Load any saved token counters and flush them back periodically so usage
	// survives restarts/reboots.
	if opts.StatePath != "" {
		rot.EnablePersistence(opts.StatePath)
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for range t.C {
				_ = rot.Persist()
			}
		}()
	}

	// Fall back to plain logging when asked or when stdout isn't a terminal (piped,
	// systemd, etc.) — the dashboard needs a real TTY.
	if opts.Headless || !isatty.IsTerminal(os.Stdout.Fd()) {
		log.Printf("chicco %s listening on %s — rotating across %d provider(s): %v", opts.Version, cfg.Addr, len(active), names)
		return http.ListenAndServe(cfg.Addr, Handler(rot, nil))
	}

	// Dashboard mode: logs flow into the on-screen pane, not stderr.
	logs := newLogBuffer(500)
	log.SetOutput(logs)
	log.SetFlags(log.Ltime)

	srv := &http.Server{Addr: cfg.Addr, Handler: Handler(rot, logs)}
	go func() {
		log.Printf("chicco %s listening on %s — %d provider(s): %v", opts.Version, cfg.Addr, len(active), names)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("chicco: server error:", err)
		}
	}()

	if _, err := tea.NewProgram(newUIModel(rot, logs, cfg.Addr), tea.WithAltScreen()).Run(); err != nil {
		return fmt.Errorf("dashboard error: %w", err)
	}
	_ = srv.Close()
	_ = rot.Persist() // final flush on clean exit
	return nil
}
