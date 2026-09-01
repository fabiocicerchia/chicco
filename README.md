# chicco

[![ci](https://github.com/fabiocicerchia/chicco/actions/workflows/ci.yml/badge.svg)](https://github.com/fabiocicerchia/chicco/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/fabiocicerchia/chicco?sort=semver)](https://github.com/fabiocicerchia/chicco/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/fabiocicerchia/chicco.svg)](https://pkg.go.dev/github.com/fabiocicerchia/chicco)
[![Go Report Card](https://goreportcard.com/badge/github.com/fabiocicerchia/chicco)](https://goreportcard.com/report/github.com/fabiocicerchia/chicco)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> *In Rome, **chicco** (pronounced "kee-kko") is an affectionate, colloquial way to
> address someone — roughly like "bro" or "dude".*

A tiny **local, OpenAI- and Anthropic-compatible rotation proxy**. chicco fronts
a pool of providers behind one stable endpoint: it forwards each request — OpenAI
chat completions (`/v1/chat/completions`), Anthropic messages (`/v1/messages`),
or embeddings (`/v1/embeddings`) — to the next provider in `chicco.yaml`,
round-robining models and skipping any that hits a quota or auth error. A single
URL cascades across your free-tier tokens and moves on as each one runs out.

Providers can be HTTP APIs **or local CLI tools** (claude, codex, gemini, qwen, …),
and both OpenAI- and Anthropic-SDK clients — agent runners, `curl`, Claude Code —
point straight at `http://127.0.0.1:41986/v1` with no code changes. (Embeddings
rotate across HTTP providers only; CLI backends return text, not vectors.)

> **Local by default, and it is a rotator, not a gateway.** chicco binds
> loopback and refuses any other address unless you set an `api_key` — it holds
> a live key for every provider in your config. It does not load-balance by
> latency or cost, it drains providers in the order you list them; and quota
> accounting is client-side bookkeeping against the limits *you* declare, not
> a reading of the provider's own counter.

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

## How it works

One process, one endpoint, and a list you control:

```
  OpenAI / Anthropic SDK, curl, Claude Code, an agent runner
      │  POST /v1/chat/completions | /v1/messages | /v1/embeddings
      ▼
  ┌───────────────────────────────────────────────────────────────┐
  │ chicco  127.0.0.1:41986                                       │
  │                                                               │
  │   auth ──────────► api_key (optional; required off loopback)  │
  │   translate ─────► Anthropic request → OpenAI shape           │
  │   pick ──────────► first provider in chicco.yaml that is      │
  │                     · not in cooldown                         │
  │                     · under its declared quota                │
  │                     · able to serve this model                │
  │   forward ───────►                                            │
  │        │                                                      │
  │        ├─ 2xx ────► relay, count tokens, mark healthy         │
  │        ├─ 429 ────► cool this provider down, try the next     │
  │        └─ 401/403 ► mark the key rejected, try the next       │
  │                                                               │
  │   translate ─────► OpenAI SSE → Anthropic SSE, or one         │
  │                     buffered Anthropic message object         │
  │   record ────────► event log → chicco-state.json (survives    │
  │                     restarts) → dashboard                     │
  └───────────────────────────────────────────────────────────────┘
      │                    │                    │
      ▼                    ▼                    ▼
  groq (HTTP)        cerebras (HTTP)      codex-cli (local CLI)
```

The list order is preference order, not a load-balancing policy: chicco drains
the first provider until it says no, then falls through. That is the whole
design — a stable URL in front of several free tiers, each used up in turn.

More in [`docs/architecture.md`](docs/architecture.md).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/fabiocicerchia/chicco/main/install.sh | sh
```

Or from a checkout:

```sh
go install github.com/fabiocicerchia/chicco@latest
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

## Configuration

`chicco.yaml`, simple and not comprehensive — the full reference is in
[`docs/configuration.md`](docs/configuration.md):

```yaml
addr: "127.0.0.1:41986"

# Guards chicco's own inbound endpoints. Empty leaves it open, which is why
# addr above is loopback-only; binding anything else without this is refused
# at startup.
# api_key: ${CHICCO_API_KEY}

providers:
  groq:
    base_url: https://api.groq.com/openai/v1
    api_key: ${GROQ_API_KEY}      # ${VAR} is expanded from the environment
    models: [llama-3.3-70b-versatile]
    quota:
      tpd: 200000                 # what chicco enforces before forwarding

  codex-cli:                      # a local CLI, not an HTTP API
    kind: cli
    command: codex
    args: ["exec", "--model", "{{model}}", "{{prompt}}"]
    models: [default]
```

`quota:` is chicco's own bookkeeping, in the units the provider caps you in —
`tpd` for token-capped tiers, `rpd` for request-capped ones. Omit it and usage
is shown without a bar.

## Usage

```console
$ chicco --help
chicco — a local OpenAI- and Anthropic-compatible rotation proxy.

chicco serves one endpoint (chat completions, messages and embeddings) and
forwards each request to the next provider in chicco.yaml — an HTTP API or a
local CLI — rotating models and skipping providers that run out of quota, so a
single stable endpoint fronts a pool of free-tier tokens.

Usage:
  chicco [flags]

Flags:
  -addr string
    	listen address (overrides chicco.yaml; default 127.0.0.1:41986)
  -check
    	validate the config and exit (no server, no port bound)
  -config string
    	path to the chicco.yaml config file (default "chicco.yaml")
  -headless
    	disable the dashboard and log plainly to stderr
  -state string
    	token-usage state file, persisted across runs (empty to disable) (default "chicco-state.json")
  -version
    	print version and exit

Example:
  chicco -config chicco.yaml -addr :41986

Then point an OpenAI client at http://127.0.0.1:41986/v1 (set the client's base URL
to this address).
```

`-check` is the one to reach for first: it parses the config, expands the
environment variables and exits, without binding a port.

## Common errors

**`chicco: addr <addr> is not loopback and no api_key is set: set api_key in chicco.yaml, or bind to 127.0.0.1`**
Refused at startup, on purpose. chicco holds a live key for every provider in
the config; exposing it unauthenticated exposes all of them. Once it is
serving, the exposure has already happened, so startup is the only place to
catch this.

**`chicco: warning: groq: no api_key (unset env var?) — provider is inactive`**
and, when it is every provider,
**`no providers with an API key and models are configured — check the providers: list and the api_key env vars in <file>`**
Usually an unexported variable rather than a missing provider: `${GROQ_API_KEY}`
expands to empty and the provider drops out. `set -a && . ./.env && set +a`
before starting, and confirm with `-check`:

```console
$ chicco -config myproviders.yaml -check
chicco: myproviders.yaml is valid
```

**`chicco: all providers exhausted: nothing attempted — ...`**
Nothing was even tried: every candidate was already in cooldown when the
request arrived. The summary after the dash says which, and why — an exhausted
quota, a rejected key and a wrong model id all land here and need different
fixes. The 503 carries a `Retry-After` (seconds until the first candidate frees
up), which the OpenAI and Anthropic SDKs honour on their own — retrying earlier
cannot succeed.

**`chicco: request sends 'tools' but every provider for this model is CLI-backed`**
CLI providers return plain text and cannot emit tool calls, so they are dropped
from the candidate list for a tool-using request rather than answering one with
prose. Give that model at least one HTTP provider.

**Usage counters look wrong after a provider reset.**
They are chicco's own bookkeeping against the `quota:` you declared, persisted
in `chicco-state.json`; nothing reads the provider's real counter. If the two
disagree, the declared window is the thing to correct.

## Documentation

Full docs live in [`docs/`](docs/). Runnable examples live in [`examples/`](examples/).

## References

Wire compatibility with two published schemas is the contract here, so the
specs are the reference and this README is not:

- [OpenAI chat completions](https://platform.openai.com/docs/api-reference/chat)
  and [embeddings](https://platform.openai.com/docs/api-reference/embeddings) —
  the request and response shapes everything is normalised to internally.
- [Anthropic messages API](https://docs.claude.com/en/api/messages) and
  [streaming events](https://docs.claude.com/en/docs/build-with-claude/streaming)
  — the event sequence `/v1/messages` has to reproduce exactly, including the
  `message_start` / `content_block_*` / `message_delta` / `message_stop` order.
- [Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
  — the framing both streams share.
- [`Retry-After`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Retry-After)
  — what a 429 is allowed to say, and where the cooldown length comes from
  when it says it.
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — the model/update
  /view contract the dashboard implements.

## Release cycle

[Semantic Versioning](https://semver.org/), cut by release-please from
[Conventional Commits](https://www.conventionalcommits.org/).

- **Major** — a change to the wire contract or to `chicco.yaml`'s schema.
- **Minor** — new providers, new flags, new dashboard behaviour.
- **Patch** — fixes; only the latest minor gets them.

Wire compatibility with the OpenAI and Anthropic schemas is never "improved":
a response shape only changes when the upstream spec changes, and that is a
major.

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). Security reports: [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE) © Fabio Cicerchia
