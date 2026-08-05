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

## Documentation

Full docs live in [`docs/`](docs/). Runnable examples live in [`examples/`](examples/).

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). Security reports: [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE) © Fabio Cicerchia
