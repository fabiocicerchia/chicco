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

## Cost

Token counts alone do not answer what a route costs. Declare prices and chicco
prices each request as it completes:

```yaml
pricing:
  currency: USD
  models:
    gpt-4o-mini:
      input_per_million: 0.15
      output_per_million: 0.60
```

Per **million** tokens, input and output separately — that is the unit every
published price list uses, and the two differ by three or four times, so a cost
computed from a total token count is wrong by whatever shape the request had.

`currency` is a label. chicco does no conversion; it reports the unit the
prices were written in, so a report cannot be read as dollars when it was
written in euros. Prices with no currency are labelled `unspecified` rather
than assumed.

Per-request cost appears in the log line (`served 812 tokens (0.0004 USD)`),
and the session total — split by provider and by model — is on `/v1/status`
under `cost`.

**Unpriced is not free.** A model with no entry is counted as unpriced and
named in the summary, never folded into the total as zero. A configured price
of `0` is the way to say a free tier costs nothing, which is a different
statement and one most of chicco's providers need.

Prices come from config rather than from the providers: there is no common API
for them, they change rarely, and a proxy that phoned home for a price list
would gain a failure mode on the request path in exchange for a number nobody
reads in the moment.

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
