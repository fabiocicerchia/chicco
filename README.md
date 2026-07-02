# chicco

[![ci](https://github.com/fabiocicerchia/chicco/actions/workflows/ci.yml/badge.svg)](https://github.com/fabiocicerchia/chicco/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/fabiocicerchia/chicco?sort=semver)](https://github.com/fabiocicerchia/chicco/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/fabiocicerchia/chicco.svg)](https://pkg.go.dev/github.com/fabiocicerchia/chicco)
[![Go Report Card](https://goreportcard.com/badge/github.com/fabiocicerchia/chicco)](https://goreportcard.com/report/github.com/fabiocicerchia/chicco)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A tiny **local OpenAI-compatible rotation proxy**. chicco serves one endpoint and
forwards each `/v1/chat/completions` request to the next free-tier provider in
`chicco.yaml`, round-robining models and skipping any provider that hits a quota
or auth error — so a single stable endpoint fronts a pool of free-tier tokens.
An Anthropic-compatible `/v1/messages` endpoint sits alongside it, translated
to and from the same rotation, so Claude Code and other Anthropic-SDK clients
can point straight at chicco too.

Point any OpenAI (or Anthropic) client (agent runner, SDK, `curl`) at
`http://127.0.0.1:41986/v1` and your requests transparently cascade across free
models, moving on to the next one as tokens run out.

It ships with a live **Bubble Tea dashboard**: the top half lists every provider
with its tokens-used / quota and a red→amber→green usage bar; the bottom half is a
rolling log pane. Token usage is **persisted to disk**, so the counters survive
restarts and reboots.

![chicco dashboard](docs/demo.gif)

The dot tells you each provider's state at a glance: **green** ready, **amber**
in cooldown after a quota/rate-limit error, **grey** if its key was rejected or the
endpoint is unreachable, hollow grey while still being checked.

Press `q` (or `esc` / `ctrl+c`) to quit. Press **`t`** to **test every configured
model**: chicco sends each one a "hello world" prompt and logs the outcome to the
log pane — which answered, token count, and each provider's window/limit (e.g.
`✓ groq/llama-3.3-70b — 44 tok · 44/200.0k tok · daily`, `✗ codex-cli/default —
limit — resets in 17h43m`). It probes every model directly (even ones in cooldown,
so you see their real state) and folds the results back into the table; runs in the
background so the dashboard stays live. Run with `-headless` (or pipe stdout) to
disable the dashboard and log plainly to stderr instead.

## Install

**One-liner** (Linux / macOS, amd64 / arm64) — downloads the latest release,
verifies its SHA-256, and installs the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/fabiocicerchia/chicco/main/install.sh | sh
```

Set `BINDIR` to choose the install directory, or `CHICCO_VERSION` to pin a tag.

**Go:**

```sh
go install github.com/fabiocicerchia/chicco/cmd/chicco@latest
```

**Manual:** grab an archive from the [releases page](https://github.com/fabiocicerchia/chicco/releases/latest).
Every release ships an SPDX SBOM and a keyless [cosign](https://github.com/sigstore/cosign)
signature over `checksums.txt`; verify it before trusting a download:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/fabiocicerchia/chicco' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

**Docker:** pull the multi-arch image (keyless-signed with cosign, same
posture as the checksums above) published by every tagged release, or build
it yourself from the repo — see [docs/DOCKER.md](docs/DOCKER.md) for the full
walkthrough (state persistence, env-var keys, signature verification,
docker-compose):

```sh
docker pull ghcr.io/fabiocicerchia/chicco:latest
docker run --rm -p 41986:41986 \
  -v "$(pwd)/chicco.yaml:/etc/chicco/chicco.yaml:ro" \
  --env-file .env \
  ghcr.io/fabiocicerchia/chicco:latest
```

## Quick start

```sh
cp chicco.yaml myproviders.yaml          # edit in your providers + keys
```

Keep your keys in a `.env` file (never commit it) rather than typing them
inline, and load it into the shell before running chicco:

```sh
# .env
GROQ_API_KEY=...
CEREBRAS_API_KEY=...
```

```sh
set -a && . ./.env && set +a
chicco -config myproviders.yaml
```

Then point any OpenAI client at `http://127.0.0.1:41986/v1`.

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

## How it works

- **Config order is preference order** (the default per-model `strategy`).
  chicco uses the first provider that isn't in cooldown, so the top entry is
  drained until it's rate-limited, then the request falls through to the next.
  List free providers first to use them before a paid fallback (or CLI tools
  first to prefer them). Within a provider its models are round-robined.
- **Other load-balancing strategies** (`round_robin`, `random`, `weighted`) can
  be set per virtual model in `models:` — see
  [docs/LOAD_BALANCING.md](docs/LOAD_BALANCING.md).
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

## Green usage

Pooling several free tiers is chicco's whole point, but it also removes the
natural stopping point any single tier would otherwise impose — you never see
"quota exceeded" until *every* configured provider is exhausted, which grows
every time you add another one. A few things worth knowing if you'd rather
keep that pooling deliberate:

- **Model size is the biggest lever, and it's already free.** Per-request
  compute scales mostly with model size and prompt/response length, far more
  than with which provider serves it. `models:` (below) already lets you list
  a virtual model's backends in preference order — put a small/fast model
  first and a larger one only as fallback.
- **The dashboard always shows a pooled total** — "today: N req · M tokens
  across P active" — in both the TUI and `/dashboard`, so the aggregate isn't
  hidden behind per-provider bars.
- **An optional top-level `quota:`** caps usage across every provider
  *combined* (see below) — a self-imposed ceiling on the pooling, not just a
  view of it.
- **Carbon-aware routing isn't offered, on purpose.** No OpenAI-compatible API
  (HTTP or CLI-backed) discloses datacenter region, PUE, or grid carbon
  intensity per request, and providers route internally and dynamically — there's
  no real signal for chicco to route on. The closest honest substitute is
  manual: rank providers in `chicco.yaml` using whatever public sustainability
  disclosures you trust, the same "config order is preference order" mechanism
  used for the free-tier-first pattern.
- **Request coalescing (de-duplicating identical concurrent calls) was
  considered and skipped.** chicco streams each response live to one caller;
  coalescing means broadcasting it to several, and non-zero-temperature
  requests are supposed to return an independent sample each call — collapsing
  two callers onto one response would silently break that. Revisit only if a
  shared multi-caller instance shows real duplicate traffic.

## Configuration — `chicco.yaml`

```yaml
addr: ":41986"          # optional; this is the default

providers:
  - name: groq
    base_url: https://api.groq.com/openai/v1
    api_key: ${GROQ_API_KEY}   # ${VAR} is expanded from the environment
    models:
      - llama-3.3-70b-versatile
  - name: cerebras
    base_url: https://api.cerebras.ai/v1
    api_key: ${CEREBRAS_API_KEY}
    models:
      - llama-3.3-70b
```

- `api_key` accepts a literal token or a `${VAR}` reference expanded from the
  environment (preferred — keep secrets out of the file).
- `quota:` (optional) sets client-side rate limits — `rpm`/`rph`/`rpd` (requests
  per minute/hour/day) and `tpm`/`tph`/`tpd` (tokens per minute/hour/day). The
  tightest daily limit also drives the dashboard's usage bar. Use `tpd` for
  token-capped providers (Groq, Cerebras) and `rpd` for request-capped ones
  (Google's free tier). Omit `quota:` to show usage without a bar.
- A top-level `quota:` (same fields as a provider's) caps usage across every
  provider *combined* — e.g. `quota: { tpd: 500000 }` stops chicco at 500k
  tokens/day total, regardless of how many providers are pooled underneath.
  Optional; omit for no aggregate cap (the default — chicco keeps draining
  providers until each is individually exhausted). The dashboard's header
  line always shows the aggregate usage whether or not a cap is set.
- A provider with an empty key or no models is inactive and skipped at startup.
- Order is the preference order. List the providers you have keys for.

### Virtual models (`models:`)

A top-level `models:` list groups backends from one or more providers under a
single virtual model ID, which callers request by name (or `chicco:auto` to
route across every active provider instead):

```yaml
models:
  - id: llama-3.3-70b
    backends:
      - provider: cerebras
        model: llama-3.3-70b
      - provider: groq
        model: llama-3.3-70b-versatile
        quota:
          tpd: 100000   # overrides groq's provider-level quota for this model only
```

Each backend can carry its own `quota:` block, which overrides the provider's
top-level quota just for that model — useful when a provider caps models
independently (e.g. Groq's per-model daily limits). A backend can likewise
carry its own `weight` (see [docs/LOAD_BALANCING.md](docs/LOAD_BALANCING.md)),
overriding the provider's weight just for that model. Omit either to inherit
the provider's own. See `chicco.yaml` for a fuller example.

### Load-balancing strategy (`strategy`)

By default chicco drains a virtual model's backends top-down (config order).
Each virtual model in `models:` can set its own `strategy:` — `round_robin`,
`random`, or `weighted` (by provider `weight`) — to spread load differently.
See [docs/LOAD_BALANCING.md](docs/LOAD_BALANCING.md) for the full reference and
examples.

### Guarding the endpoint (`api_key`)

chicco has no inbound authentication by default — anyone who can reach `addr`
can use it, which is fine on the `127.0.0.1` default. If you bind a public
address, set a top-level `api_key` so callers must present it:

```yaml
api_key: ${CHICCO_API_KEY}   # inbound shared secret; ${VAR} expanded from env
```

Every endpoint except `/health` (kept open for liveness probes) then requires
`Authorization: Bearer <key>`; the token is compared in constant time. Point
your OpenAI client at chicco with this key as its API key.

## CLI-backed providers (`kind: cli`)

A provider can run a **local CLI tool** (claude, codex, kiro, a qwen CLI, …) instead
of making an HTTP call, appearing as just another model behind the same
`:41986/v1` endpoint with the same cooldown / failover / dashboard behaviour.
Adding a new tool is a YAML entry, no code. See
[docs/CLI_PROVIDERS.md](docs/CLI_PROVIDERS.md) for the field reference, auth/limit
detection, and the no-tools-off caveat — and `chicco.yaml` for ready presets
(claude, codex, gemini, qwen, kiro).

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

## Pointing an agent at chicco

chicco is a plain OpenAI-compatible endpoint, so any client that lets you set a
base URL works — point it at `http://127.0.0.1:41986/v1` (no API key needed).
The [`examples/`](examples/) folder has ready configs for **OpenCode**,
**Continue**, **Aider**, **Headroom** (context compression in front of chicco),
and the raw OpenAI SDK / `curl`.

Note: chicco overrides the request's `model` field with its rotation pick, so the
model name a client sends is arbitrary.

## Build

```sh
make build      # -> ./bin/chicco
make test       # go test -race ./...
make snapshot   # local GoReleaser build (dist/) — needs goreleaser
```

See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for all make targets and the
local-development requirements.

## Endpoints

| Method | Path                    | Purpose                                          |
|--------|-------------------------|--------------------------------------------------|
| `POST` | `/v1/chat/completions`  | rotated, proxied chat completion (OpenAI-shaped) |
| `POST` | `/v1/messages`          | rotated, proxied chat completion (Anthropic-shaped) |
| `GET`  | `/v1/models`            | list virtual model IDs + `chicco:auto`           |
| `GET`  | `/v1/status`            | JSON snapshot of all providers + recent logs (web dashboard data source) |
| `GET`  | `/health`               | liveness probe (returns `200`)                   |
| `GET`  | `/dashboard`            | live web dashboard — mirrors the TUI in a browser, polling `/v1/status` every second |

When the `model` field in a `/v1/chat/completions` request matches a virtual model
ID from the `models:` section of `chicco.yaml`, the request is routed only to the
backends configured for that model. Use `chicco:auto` (or any unknown model name)
to route across all active providers regardless of model.

`GET /v1/status` is also useful for a `-headless` deployment that stays
observable without a terminal:

```sh
curl -s localhost:41986/v1/status | jq
```

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). Security reports: [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE) © Fabio Cicerchia
