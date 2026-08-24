# Running chicco

## Boot health check

At startup chicco probes every active provider's `GET /v1/models` with its key —
a free call that spends no quota — to confirm the endpoint is live and the token
is valid. A provider whose key is missing or rejected (`401`/`403`) is greyed in
the dashboard immediately, before any real request is routed, so a dead key is
obvious instead of surfacing later as a failed completion. The same grey state is
applied at request time if a provider returns an auth error, and cleared back to
green on the next successful response. The probe **re-runs every 5 minutes**, so a
provider that was down at boot (network not up yet) or transiently rate-limited
recovers to green on its own. CLI providers are probed by their `health_command`
(or `credential` file) instead.

```
client ──HTTP──▶ chicco (:41986) ──▶ groq      (llama-3.3-70b-versatile)
                                 ├──▶ cerebras  (llama-3.3-70b)
                                 ├──▶ openrouter(deepseek-chat-v3:free)
                                 └──▶ google    (gemini-2.0-flash)
```

## Token accounting & persistence

chicco asks each streamed request for a usage summary (`stream_options.include_usage`)
and reads `usage.total_tokens` from the response to tally per-provider consumption.
Counters are written to a JSON **state file** (`-state`, default `chicco-state.json`)
atomically every 10s and on a clean dashboard exit, so usage persists across runs and
reboots. Pass `-state ""` to disable persistence.

> The dashboard bar tracks whichever `quota:` field is set: `rpd`/`tpd` resets at
> UTC midnight; `rph`/`tph` and `rpm`/`tpm` are rolling windows (the last hour /
> minute of events). With no `quota:` set, usage accumulates forever (delete the
> state file to zero it). A provider that doesn't report `usage` simply
> contributes 0 tokens for that request.

It's a plain JSON file by design — the data is a tiny per-provider counter map, so
SQLite would add a heavy dependency for no real gain.

## Running

```sh
chicco -config chicco.yaml       # keys loaded from .env — see Quick start

# or straight from a checkout
go run ./cmd/chicco -config chicco.yaml
```

Flags:

| Flag         | Default              | Meaning                                          |
|--------------|----------------------|--------------------------------------------------|
| `-config`    | `chicco.yaml`        | path to the config file                          |
| `-addr`      | (from config)        | listen address, overrides `chicco.yaml`          |
| `-state`     | `chicco-state.json`  | token-usage state file (empty disables it)       |
| `-headless`  | `false`              | disable the dashboard; log plainly to stderr     |
| `-check`     | `false`              | validate the config and exit (no server)         |
| `-version`   | —                    | print version and exit                           |

`chicco -check -config chicco.yaml` statically validates the config — bad YAML,
a `kind: cli` provider missing `command`, an `http` provider missing `base_url`,
unknown `kind`/`output`/model `strategy` values, duplicate names — and exits
non-zero on any hard error (warnings for inactive providers don't fail it). It
binds no port, so it's safe in CI or a pre-commit hook.

`chicco -help` prints full usage.

## Metrics

Off by default. Enabled, chicco serves a Prometheus exposition on **its own
listener** — the proxy port is what an agent runner points at, and a scrape
target is a different audience:

```yaml
metrics:
  enabled: true
  addr: "127.0.0.1:41987"   # default when omitted
```

```
chicco_requests_total{provider,model}          successful upstream calls
chicco_tokens_total{provider,model}            tokens as reported upstream
chicco_upstream_errors_total{provider,status}  failures; status="transport" when there was none
chicco_provider_blocks_total{provider,reason}  cooldowns entered: limit | auth | error
chicco_upstream_latency_seconds{provider}      histogram, successful calls and failures alike
chicco_providers_blocked                       gauge: providers in cooldown right now
```

Labels are bounded by the config — provider names, model ids, and a fixed set of
reasons and statuses. Nothing request-derived is ever a label, so a caller cannot
grow the series count, and no prompt or key can reach the scrape target.

The endpoint has no auth of its own, for the same reason `/health` has none:
bind it where only the scraper can reach it. The default address is loopback.
## Budget alerts

Quota exhaustion is otherwise discovered when requests start failing. In
chicco that failure is quiet by design — the rotation moves on — which is also
how a whole pool of free tiers disappears over an afternoon with nothing said
until the last one goes.

```yaml
alerts:
  threshold_percent: 80
  webhook: https://hooks.example.com/chicco   # optional
  webhook_timeout: 5s
```

```
chicco: groq at 82% of its daily quota (8200/10000 tokens) — threshold 80%
```

- **Once per crossing, not per request.** Firing per request emits hundreds of
  lines for one event, which trains everyone to ignore them.
- **Re-arms when the window rolls.** De-duplication and reset are the same
  mechanism: an alert is remembered against the instant its window began, and a
  new window has a different start. A daily quota that warned at 4pm can warn
  again tomorrow; one with no window warns once for the process lifetime.
- **The payload carries no request content** — provider, threshold, usage,
  unit, window, timestamp. A webhook is an outbound call to a third party, and
  the payload is the part that leaks if it is ever pointed at the wrong host.
- **A failed webhook is logged, never returned.** The post runs in its own
  goroutine off the request path, so a dead notification endpoint cannot fail
  somebody's completion.

## Reloading the config (SIGHUP)

Edit `chicco.yaml` — rotate a key, add or remove a provider, change a model's
`strategy` — and send chicco a `SIGHUP` to apply it **without a restart**:

```sh
kill -HUP $(pgrep chicco)
```

The config is re-read and validated; a parse error or a hard validation problem
is logged and the running config is kept, so a bad edit never takes chicco down.
Providers that survive the edit keep their live usage counters and cooldowns; a
removed provider is dropped and a new one starts fresh and is health-probed. The
listen address can't change this way (the socket is already bound) — that still
needs a restart. SIGHUP is a no-op on Windows.
