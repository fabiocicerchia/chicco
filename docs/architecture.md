# Architecture

## Code layout

`cmd/chicco` is a thin shell — it parses flags and calls `proxy.Run`. Everything
else lives in `internal/proxy`, one file per job:

| Area | Files |
|------|-------|
| Startup and process | `run.go`, `reload.go` + `reload_unix.go` / `reload_windows.go` |
| Config | `config.go` (shape), `configload.go` (read + expand), `validate.go` (`-check`) |
| Rotation state | `rotator.go` (the `Rotator`), `state.go` (persistence), `stats.go` (snapshots) |
| Choosing a backend | `route.go` (candidates, strategies, pick), `ratelimit.go` (quota windows) |
| Serving a request | `proxy.go` (endpoints), `auth.go` (inbound key), `dispatch.go` (failover), `classify.go` (what a non-2xx means) |
| Anthropic wire format | `anthropic.go` (`/v1/messages`), `anthropic_request.go` (in), `anthropic_stream.go` + `anthropic_sse.go` + `anthropic_json.go` (out) |
| CLI providers | `cli.go` (run it), `clierrors.go` (read the failure), `clisynth.go` (synthesize the OpenAI reply) |
| Dashboards | `ui.go` + `uirender.go` + `uiprovider.go` (terminal), `web.go` + `dashboard.html` (browser), `logbuffer.go` (shared log ring) |
| Observability | `metrics.go`, `cost.go`, `alert.go`, `health.go`, `test.go` |

The browser dashboard is a single self-contained `dashboard.html`, compiled into
the binary with `//go:embed` — there are no asset routes and nothing to deploy
beside the binary.

## How it works

- **Config order is preference order** (the default per-model `strategy`).
  chicco uses the first provider that isn't in cooldown, so the top entry is
  drained until it's rate-limited, then the request falls through to the next.
  List free providers first to use them before a paid fallback (or CLI tools
  first to prefer them). Within a provider its models are round-robined.
- **Other load-balancing strategies** (`round_robin`, `random`, `weighted`) can
  be set per virtual model in `models:` — see
  [docs/LOAD_BALANCING.md](LOAD_BALANCING.md).
- The requested `model` field is **overridden** with the rotation's pick — callers
  send any model name; chicco decides. Every other field (temperature, samplers,
  `response_format`, `stream`) passes through untouched.
- If a provider returns a non-2xx status, chicco puts it in **cooldown** and
  retries the request on the next provider:
  - `401` / `403` (bad key) → 1 hour
  - `429` / other → the `Retry-After` header if given, else 1 minute
- Failover happens on the upstream's initial status (where quota/auth errors
  surface). Once a provider starts streaming a `200`, its response is proxied
  straight back to the caller, flushed chunk by chunk.
- When every provider is exhausted or in cooldown, chicco returns a `503` with an
  OpenAI-style error envelope.

chicco relies on each provider's own `429` to tell it a free tier is spent; token
counters are persisted (see below).

## Sustainable Usage

Pooling free tiers is the point, but it also removes any single tier's natural
stop: `429`-driven failover only trips once *every* provider is exhausted, and
that ceiling rises with each one you add. Levers to keep the pooling deliberate:

- **Model size dominates energy per request**, far more than provider choice —
  inference cost scales with active parameters × (prompt + completion) tokens.
  Order a virtual model's `backends:` smallest-first so the cheapest model
  serves by default and larger ones are fallback only (config order = routing
  order; see [Virtual models](#virtual-models-models)).
- **Cap the pool, don't just watch it.** A top-level `quota:` (`tpd`/`rpd`/…)
  enforces a hard aggregate ceiling across all providers; the TUI and
  `/dashboard` header always show pooled `today: N req · M tokens across P
  active` whether or not a cap is set.
- **No carbon-aware routing** — no OpenAI/Anthropic API (HTTP or CLI) exposes
  region, PUE, or marginal grid intensity per request, and providers route
  internally, so there's no signal to optimize on. The honest substitute is
  manual: rank providers in `chicco.yaml` by whatever disclosures you trust.
- **No request coalescing** — chicco streams each response to a single caller;
  fanning one completion out to several would break the independent-sample
  guarantee of `temperature > 0`.
