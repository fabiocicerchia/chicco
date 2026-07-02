# Load-balancing strategy (`strategy`)

By default chicco drains a virtual model's backends top-down (config order). Set
a per-model `strategy:` under `models:` in `chicco.yaml` to spread load
differently:

```yaml
providers:
  groq:
    weight: 3   # only used by "weighted": picked ~3× as often as weight 1
    # …
  cerebras:
    # …

models:
  - id: llama-3.3-70b
    strategy: weighted     # order (default) | round_robin | random | weighted
    backends:
      - provider: groq
      - provider: cerebras
```

| `strategy` | behaviour |
|---|---|
| `order` (default) | config order — drain the top backend, then fall through |
| `round_robin` | rotate the starting backend each request; load spreads evenly |
| `random` | a fresh random order each request |
| `weighted` | random order biased by each backend provider's `weight` (unset = 1) |

Each virtual model can use a different strategy. A request that doesn't match any
model in `models:` (`chicco:auto`, or an unknown model name) always uses plain
config order across all active providers — there's no virtual model to carry a
strategy.

Whatever the strategy, chicco still skips providers in cooldown and fails over
down the list — a strategy only changes *preference*, never correctness.

`weight` is set per-provider by default, since it usually describes a property
of the provider itself. A backend can override it for one virtual model only —
useful when the same provider should be weighted differently across models:

```yaml
models:
  - id: llama-3.3-70b
    strategy: weighted
    backends:
      - provider: groq
        weight: 3   # overrides groq's provider-level weight for this model only
      - provider: cerebras
```

Omit `weight` on a backend to inherit the provider's own weight.
