// Package proxy implements chicco's local OpenAI-compatible rotation proxy: it
// serves one /v1/chat/completions endpoint and forwards each request to the next
// free-tier provider in the config, round-robining models and skipping providers
// that hit a quota/rate-limit (429) or auth (401/403) error until one answers.
package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// readHeaderTimeout bounds how long a client gets to send request headers,
// closing off slowloris-style connections that trickle bytes to hold a socket
// open. It only guards the header read, not the body or a streamed response,
// so long-running chat completions are unaffected.
const readHeaderTimeout = 10 * time.Second

// RequireAuthOnBind refuses to let chicco be reachable and unauthenticated at the
// same time. chicco holds a working API key for every configured provider, so an
// open listener on a routable address hands those keys to anything that can reach
// the port. Startup is the only place to catch it — once serving, the exposure has
// already happened.
func RequireAuthOnBind(addr, apiKey string) error {
	if apiKey != "" || isLoopback(addr) {
		return nil
	}
	return fmt.Errorf("chicco: addr %s is not loopback and no api_key is set: "+
		"set api_key in chicco.yaml, or bind to 127.0.0.1", addr)
}

// authState describes the inbound auth posture for the startup log, so "is this
// thing open?" is answerable from the log line rather than by reading the config.
func authState(apiKey string) string {
	if apiKey == "" {
		return "no api_key — open"
	}
	return "api_key set"
}

// isLoopback reports whether addr binds only the loopback interface. A bare
// ":41986" or "0.0.0.0:41986" is not loopback: it listens on every interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
	if err := RequireAuthOnBind(cfg.Addr, cfg.APIKey); err != nil {
		return err
	}

	rot := NewRotator(cfg.Providers, cfg.Models)
	rot.authKey = cfg.APIKey // shared secret guarding inbound requests, if set
	rot.quota = cfg.Quota    // optional cap across every provider combined, if set
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

	// Reload chicco.yaml on SIGHUP (add/rotate a provider or key, change strategy)
	// without a restart — the listen address can't change (socket already bound), but
	// everything else does, keeping live counters/cooldowns. No-op on Windows.
	watchSIGHUP(context.Background(), rot, opts.ConfigPath)

	// Load any saved token counters and flush them back periodically so usage
	// survives restarts/reboots.
	if opts.StatePath != "" {
		rot.EnablePersistence(opts.StatePath)
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			// Report a failing write ONCE. Discarding this error hid an unwritable
			// state directory for as long as it existed: usage counters and
			// rate-limit windows silently reset on every restart, which lets a
			// daily quota be re-spent by restarting. Once, not every tick, so a
			// permanent failure can't flood the log or the dashboard pane.
			var warned bool
			for range t.C {
				if err := rot.Persist(); err != nil && !warned {
					warned = true
					log.Printf("chicco: WARNING: cannot write state to %s (%v) — usage counters "+
						"and rate-limit windows will reset on restart", opts.StatePath, err)
				}
			}
		}()
	}

	// Always keep a log buffer so /v1/status (the web dashboard) has something to
	// show even headless — it's the only UI in that mode, so its logs panel can't
	// rely on the TUI being the one populating it.
	logs := newLogBuffer(500)

	// Fall back to plain logging when asked or when stdout isn't a terminal (piped,
	// systemd, etc.) — the dashboard needs a real TTY.
	if opts.Headless || !isatty.IsTerminal(os.Stdout.Fd()) {
		log.SetOutput(io.MultiWriter(os.Stderr, logs)) // keep stderr; also feed the web dashboard
		log.Printf("chicco %s listening on %s (%s) — rotating across %d provider(s): %v", opts.Version, cfg.Addr, authState(cfg.APIKey), len(active), names)
		srv := &http.Server{Addr: cfg.Addr, Handler: Handler(rot, logs), ReadHeaderTimeout: readHeaderTimeout}
		return srv.ListenAndServe()
	}

	// Dashboard mode: logs flow into the on-screen pane, not stderr.
	log.SetOutput(logs)
	log.SetFlags(log.Ltime)

	srv := &http.Server{Addr: cfg.Addr, Handler: Handler(rot, logs), ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		log.Printf("chicco %s listening on %s (%s) — %d provider(s): %v", opts.Version, cfg.Addr, authState(cfg.APIKey), len(active), names)
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
