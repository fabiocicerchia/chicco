# Configuration

## Configuration — `chicco.yaml`

```yaml
addr: "127.0.0.1:41986" # optional; this is the default — loopback only

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
carry its own `weight` (see [docs/LOAD_BALANCING.md](LOAD_BALANCING.md)),
overriding the provider's weight just for that model. Omit either to inherit
the provider's own. See `chicco.yaml` for a fuller example.

### Load-balancing strategy (`strategy`)

By default chicco drains a virtual model's backends top-down (config order).
Each virtual model in `models:` can set its own `strategy:` — `round_robin`,
`random`, or `weighted` (by provider `weight`) — to spread load differently.
See [docs/LOAD_BALANCING.md](LOAD_BALANCING.md) for the full reference and
examples.

### Guarding the endpoint (`api_key`)

chicco has no inbound authentication by default — anyone who can reach `addr`
can use it, and use it to spend every provider key chicco holds. That is why the
default `addr` is loopback-only. **Binding anywhere else without an `api_key` is
refused at startup**, since serving is too late to catch it:

```
chicco: addr 0.0.0.0:41986 is not loopback and no api_key is set:
set api_key in chicco.yaml, or bind to 127.0.0.1
```

To bind a routable address, set a top-level `api_key` so callers must present it:

```yaml
api_key: ${CHICCO_API_KEY}   # inbound shared secret; ${VAR} expanded from env
```

Every endpoint except `/health` (kept open for liveness probes) then requires
`Authorization: Bearer <key>`; the token is compared in constant time. Point
your OpenAI client at chicco with this key as its API key.
