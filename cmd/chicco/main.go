// Command chicco is a local OpenAI-compatible rotation proxy. It listens on one
// endpoint (default :41986) and forwards each /v1/chat/completions request to the
// next upstream free-tier provider listed in chicco.yaml, round-robining models
// and skipping providers that hit a quota/rate-limit (429) or auth (401/403)
// error until one answers. Point any OpenAI client at http://127.0.0.1:41986/v1
// and free-tier tokens rotate underneath a single stable endpoint. See the
// README for details.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fabiocicerchia/chicco/internal/proxy"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", "chicco.yaml", "path to the chicco.yaml config file")
	addr := flag.String("addr", "", "listen address (overrides chicco.yaml; default "+proxy.DefaultAddr+")")
	statePath := flag.String("state", "chicco-state.json", "token-usage state file, persisted across runs (empty to disable)")
	headless := flag.Bool("headless", false, "disable the dashboard and log plainly to stderr")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("chicco", version)
		return
	}

	if err := proxy.Run(proxy.Options{
		ConfigPath: *cfgPath,
		Addr:       *addr,
		StatePath:  *statePath,
		Headless:   *headless,
		Version:    version,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "chicco:", err)
		os.Exit(1)
	}
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

Then point an OpenAI client at http://127.0.0.1%s/v1 (set the client's base URL
to this address).
`, proxy.DefaultAddr)
}
