# chicco

A tiny **local OpenAI-compatible rotation proxy**. chicco serves one endpoint and
forwards each `/v1/chat/completions` request to the next free-tier provider in
`chicco.yaml`, round-robining models and skipping any provider that hits a quota
or auth error — so a single stable endpoint fronts a pool of free-tier tokens.

It is the companion to [ciccio](../README.md): point ciccio's `openai` backend at
chicco and your agents transparently cascade across free models, moving on to the
next one as tokens run out.

It ships with a live **Bubble Tea dashboard**: the top half lists every provider
with its tokens-used / quota and a red→amber→green usage bar; the bottom half is a
rolling log pane. Token usage is **persisted to disk**, so the counters survive
restarts and reboots.

```
╭ chicco · :41986 · 5 providers ───────────────────────────────────╮
│   STATUS               TOKENS USED / QUOTA   REQS   USAGE          │
│ ● groq                 123.4k / 200k         req 18 [████████░░] 62%│
│ ● cerebras             88.0k / 1.0M          req 12 [█░░░░░░░░░]  9%│
│ ◐ openrouter           —                     req 4  cooldown 47s   │
│ ● google               12.0k / —             req 3  (no quota)     │
│ ● mistral              0 / —                 req 0  auth failed — check API key │
│ ● ready   ◐ cooldown   ● bad key / down   ○ checking   q quits     │
╰───────────────────────────────────────────────────────────────────╯
╭ logs ─────────────────────────────────────────────────────────────╮
│ 17:53:47 chicco: routing to groq (llama-3.3-70b-versatile)         │
│ 17:53:49 chicco: groq (llama-3.3-70b-versatile) served 412 tokens  │
╰───────────────────────────────────────────────────────────────────╯
```

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
ciccio ──HTTP──▶ chicco (:41986) ──▶ groq      (llama-3.3-70b-versatile)
                                 ├──▶ cerebras  (llama-3.3-70b)
                                 ├──▶ openrouter(deepseek-chat-v3:free)
                                 └──▶ google    (gemini-2.0-flash)
```

## How it works

- **Config order is preference order.** chicco uses the first provider that isn't
  in cooldown, so the top entry is drained until it's rate-limited, then the request
  falls through to the next. List free providers first to use them before a paid
  fallback (or CLI tools first to prefer them). Within a provider its models are
  round-robined.
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
- `quota_tokens` / `quota_requests` (optional) drive the dashboard's usage bar.
  Tokens take priority; use `quota_requests` for providers capped by request
  count rather than tokens (e.g. Google's free tier). Omit both to show usage
  without a bar.
- `window` (`daily` | `monthly` | `none`, default `none`) resets the usage
  counters at that boundary (UTC), so the bar tracks the real remaining quota
  instead of growing forever. The window start is persisted, so a restart within
  the same window keeps the count.
- A provider with an empty key or no models is inactive and skipped at startup.
- Order is the preference order. List the providers you have keys for.

## CLI-backed providers (`kind: cli`)

A provider can run a **local CLI tool** (claude, codex, kiro, a qwen CLI, …) instead
of making an HTTP call. chicco builds the command from a template, runs it once,
buffers the completion, and synthesizes the OpenAI SSE the caller expects — so a CLI
tool appears as just another model behind the same `:41986/v1` endpoint, with the
same cooldown / failover / dashboard behaviour. Adding a new tool is a YAML entry,
no code.

```yaml
  - name: claude-cli
    kind: cli
    command: claude
    args: ["-p", "{{prompt}}", "--model", "{{model}}", "--bare", "--tools", "",
           "--system-prompt", "{{system}}", "--output-format", "json"]
    output: json                 # parse the tool's JSON…
    result_path: result          # …text is here
    tokens_path: usage.output_tokens   # …token count here (optional)
    health_command: ["claude", "--version"]   # exit 0 = healthy
    models: [claude-sonnet-4-6, claude-opus-4-8]
```

| field | meaning |
|---|---|
| `command` / `args` | the tool and its argv; `${VAR}` is env-expanded |
| placeholders | `{{model}}` `{{system}}` `{{user}}` `{{prompt}}` (system+user) `{{output_file}}` |
| `prompt_stdin` | pipe `{{prompt}}` on stdin instead of as an arg |
| `output_file` | read the answer from the temp `{{output_file}}` path (codex) |
| `output` | `text` (default, stdout verbatim) or `json` |
| `result_path` / `tokens_path` | dotted JSON paths when `output: json` |
| `error_path` | dotted JSON path that, when truthy, marks the call failed → cooldown + fail over (e.g. claude's `is_error`) |
| `strip_ansi` | strip colour codes from output (kiro) |
| `health_command` / `health_expect` | health: run a local auth-status command; greys (auth) when it exits non-zero or its output lacks `health_expect` (e.g. `["claude","auth","status"]` + `'"loggedIn": true'`) |
| `credential` | fallback health when no `health_command`: stat a file (missing = needs login) |
| `timeout_seconds` | CLI run timeout (default 120) |

CLI providers need no `api_key` — they authenticate via the tool's own login. A
failed run is treated like a flaky upstream: the provider is cooled down and the
request fails over to the next. chicco reads the failure message:

- an **auth** problem (*not logged in*, *unauthorized*, …) **greys** the provider
  (the same as a bad HTTP key);
- a **usage-limit** hit (*limit reached*, *rate limit*, *quota*, …) cools it down
  **until the window reopens** — chicco parses the reset time from the message
  (*"resets in 2h 30m"*, *"try again in 45 minutes"*, *"resets at 3pm"*) and the
  dashboard's `cd …` countdown then shows **when the next window is available**
  (falling back to 1h when no time is given). These CLIs expose no free "when does
  my quota reset" command, so this is detected from the limit error, not polled.

When a tool doesn't report token usage, chicco estimates it (`≈ len/4`) so the
dashboard bar still moves. See `chicco.yaml` for ready presets.

**Login state in the dashboard.** With a `health_command` (a free local
auth-status check like `claude auth status`), the boot/periodic probe shows the
provider's *real* login state — green only when actually logged in. Without one,
chicco falls back to stat'ing the `credential` file, which only proves the tool is
*set up*, not logged in; such a provider shows green until a real request fails
auth and greys it (see above).

**Tools are text-in / text-out — keep the CLI's own tools off.** chicco flattens
the request to one prompt and reads back plain text; it does not support OpenAI
function-calling (a `tools` array in the request is ignored, with a logged warning).
More importantly, run each CLI in a **no-tools / read-only** mode so it can't edit
files or run commands on the host — any edits would land in chicco's working
directory, and ciccio expects to apply edits itself from the returned text. The
presets do this where the tool allows it (claude `--bare --tools ""`, codex
`--sandbox read-only`, qwen plain `-p`); kiro has no clean answer-only mode, so it's
the least suitable here.

## Token accounting & persistence

chicco asks each streamed request for a usage summary (`stream_options.include_usage`)
and reads `usage.total_tokens` from the response to tally per-provider consumption.
Counters are written to a JSON **state file** (`-state`, default `chicco-state.json`)
atomically every 10s and on a clean dashboard exit, so usage persists across runs and
reboots. Pass `-state ""` to disable persistence.

> Counters reset at each provider's `window` boundary (daily/monthly UTC); with
> `window: none` they accumulate forever (delete the state file to zero them). A
> provider that doesn't report `usage` simply contributes 0 tokens for that request.

It's a plain JSON file by design — the data is a tiny per-provider counter map, so
SQLite would add a heavy dependency for no real gain.

## Running

```sh
# from this directory
GROQ_API_KEY=... CEREBRAS_API_KEY=... chicco

# or from the repo root
go run ./chicco -config chicco/chicco.yaml
```

Flags:

| Flag         | Default              | Meaning                                          |
|--------------|----------------------|--------------------------------------------------|
| `-config`    | `chicco.yaml`        | path to the config file                          |
| `-addr`      | (from config)        | listen address, overrides `chicco.yaml`          |
| `-state`     | `chicco-state.json`  | token-usage state file (empty disables it)       |
| `-headless`  | `false`              | disable the dashboard; log plainly to stderr     |
| `-version`   | —                    | print version and exit                           |

`chicco -help` prints full usage.

## Pointing ciccio at chicco

In `ciccio.yaml`:

```yaml
default_provider: openai
provider:
  openai:
    options:
      baseURL: http://127.0.0.1:41986/v1
      apiKey: ""        # not needed for a local proxy
```

Now every ciccio agent runs through the free-tier cascade.

## Build

```sh
make build-chicco       # -> ./bin/chicco
go build -o bin/chicco ./chicco
go test ./chicco/
```

## Endpoints

| Method | Path                    | Purpose                          |
|--------|-------------------------|----------------------------------|
| `POST` | `/v1/chat/completions`  | rotated, proxied chat completion |
| `GET`  | `/health`               | liveness probe (returns `200`)   |
