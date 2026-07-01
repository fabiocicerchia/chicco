# Pointing agents at chicco

chicco is just an OpenAI-compatible endpoint, so any client that lets you set a
base URL works. Start chicco first:

```sh
chicco -config chicco.yaml     # listens on http://127.0.0.1:41986/v1
```

Two things hold for **every** example below:

- **The model name is arbitrary.** chicco overrides the request's `model` field
  with its own rotation pick, so send any string — `chicco`, `auto`, whatever.
- **No API key is needed.** chicco does no auth of its own; where a client insists
  on a non-empty key, use any placeholder (`sk-local`).

---

## OpenCode

[`opencode.json`](opencode.json) — put it in your project root or `~/.config/opencode/opencode.json`,
then pick the `chicco` model with `/models`. OpenCode also has native support
(`headroom wrap opencode`) if you want compression in front — see Headroom below.

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "chicco": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "chicco (local rotation proxy)",
      "options": { "baseURL": "http://127.0.0.1:41986/v1", "apiKey": "sk-local" },
      "models": { "chicco": { "name": "chicco (rotates across free tiers)" } }
    }
  }
}
```

Docs: <https://opencode.ai/docs/providers/>

---

## Headroom (context-compression proxy, in front of chicco)

[Headroom](https://github.com/chopratejas/headroom) compresses tool output and
context before it hits the model. It's itself a proxy, so you chain it *in front*
of chicco — point Headroom's OpenAI upstream at chicco with `OPENAI_TARGET_API_URL`:

```sh
OPENAI_TARGET_API_URL=http://127.0.0.1:41986/v1 headroom proxy --port 8787
```

```
your agent ──▶ headroom :8787 (compresses) ──▶ chicco :41986 (rotates) ──▶ providers
```

Then point your OpenAI client at `http://127.0.0.1:8787/v1` instead of chicco
directly. (`OPENAI_TARGET_API_URL` is read by Headroom's proxy — see
`headroom/proxy/server.py`.)

Docs: <https://github.com/chopratejas/headroom>

---

## Aider

Aider talks to any OpenAI-compatible endpoint via LiteLLM's `openai/` prefix:

```sh
export OPENAI_API_BASE=http://127.0.0.1:41986/v1
export OPENAI_API_KEY=sk-local
aider --model openai/chicco
```

Docs: <https://aider.chat/docs/llms/openai-compat.html>

---

## Continue (VS Code / JetBrains)

[`continue.config.yaml`](continue.config.yaml) — merge into `~/.continue/config.yaml`.

```yaml
models:
  - name: chicco (local rotation proxy)
    provider: openai
    model: chicco
    apiBase: http://127.0.0.1:41986/v1
    apiKey: sk-local
    roles: [chat, edit, apply]
```

Docs: <https://docs.continue.dev/customize/model-providers/top-level/openai>

---

## Anything else (raw OpenAI SDK / curl)

Most SDKs read `OPENAI_BASE_URL` (or take a `base_url` argument):

```sh
export OPENAI_BASE_URL=http://127.0.0.1:41986/v1
export OPENAI_API_KEY=sk-local
```

```sh
curl http://127.0.0.1:41986/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"chicco","messages":[{"role":"user","content":"hello"}]}'
```
