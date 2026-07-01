// Command chicco is a local OpenAI-compatible rotation proxy. It listens on one
// endpoint (default :41986) and forwards each /v1/chat/completions request to the
// next upstream free-tier provider listed in chicco.yaml, round-robining models
// and skipping providers that hit a quota/rate-limit (429) or auth (401/403)
// error until one answers. Point any OpenAI client — such as ciccio's `openai`
// backend — at http://127.0.0.1:41986/v1 and free-tier tokens rotate underneath a
// single stable endpoint. See chicco/README.md for details.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", "chicco.yaml", "path to the chicco.yaml config file")
	addr := flag.String("addr", "", "listen address (overrides chicco.yaml; default "+DefaultAddr+")")
	statePath := flag.String("state", "chicco-state.json", "token-usage state file, persisted across runs (empty to disable)")
	headless := flag.Bool("headless", false, "disable the dashboard and log plainly to stderr")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("chicco", version)
		return
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chicco:", err)
		os.Exit(1)
	}
	if *addr != "" {
		cfg.Addr = *addr
	}

	rot := NewRotator(cfg.Providers)
	active := rot.Active()
	if len(active) == 0 {
		fmt.Fprintln(os.Stderr, "chicco: no providers with an API key and models are configured —")
		fmt.Fprintln(os.Stderr, "        check the `providers:` list and the api_key env vars in", *cfgPath)
		os.Exit(1)
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
	if *statePath != "" {
		rot.EnablePersistence(*statePath)
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
	if *headless || !isatty.IsTerminal(os.Stdout.Fd()) {
		log.Printf("chicco %s listening on %s — rotating across %d provider(s): %v", version, cfg.Addr, len(active), names)
		if err := http.ListenAndServe(cfg.Addr, Handler(rot)); err != nil {
			log.Fatalln("chicco:", err)
		}
		return
	}

	// Dashboard mode: logs flow into the on-screen pane, not stderr.
	logs := newLogBuffer(500)
	log.SetOutput(logs)
	log.SetFlags(log.Ltime)

	srv := &http.Server{Addr: cfg.Addr, Handler: Handler(rot)}
	go func() {
		log.Printf("chicco %s listening on %s — %d provider(s): %v", version, cfg.Addr, len(active), names)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("chicco: server error:", err)
		}
	}()

	if _, err := tea.NewProgram(newUIModel(rot, logs, cfg.Addr), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "chicco: dashboard error:", err)
	}
	_ = srv.Close()
	_ = rot.Persist() // final flush on clean exit
}

func usage() {
	fmt.Fprintf(os.Stderr, `chicco — a local OpenAI-compatible rotation proxy.

chicco serves one OpenAI-compatible endpoint and forwards each chat-completion
request to the next free-tier provider in chicco.yaml, rotating models and
skipping providers that run out of quota, so a single stable endpoint fronts a
pool of free-tier tokens.

Usage:
  chicco [flags]

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Example:
  chicco -config chicco.yaml -addr :41986

Then point an OpenAI client at http://127.0.0.1%s/v1 (in ciccio.yaml set
provider.openai.options.baseURL and default_provider: openai).
`, DefaultAddr)
}
