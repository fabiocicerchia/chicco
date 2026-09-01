# Endpoints

## Endpoints

| Method | Path                    | Purpose                                          |
|--------|-------------------------|--------------------------------------------------|
| `POST` | `/v1/chat/completions`  | rotated, proxied chat completion (OpenAI-shaped) |
| `POST` | `/v1/messages`          | rotated, proxied chat completion (Anthropic-shaped) |
| `POST` | `/v1/embeddings`        | rotated, proxied embeddings (OpenAI-shaped); HTTP providers only |
| `GET`  | `/v1/models`            | list virtual model IDs + `chicco:auto`           |
| `GET`  | `/v1/status`            | JSON snapshot of all providers + recent logs (web dashboard data source) |
| `GET`  | `/health`               | liveness probe: always `200`, body reports each provider's state |
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

When an inbound `api_key` is set, a browser has no way to send the
`Authorization: Bearer <key>` header the other endpoints expect, so `/dashboard`
takes the key once from the query string instead:

```
http://localhost:41986/dashboard?key=<your api_key>
```

chicco redirects to the bare `/dashboard` and stores the key in a `HttpOnly`,
`SameSite=Strict` cookie, which then authenticates the page and its `/v1/status`
polling until the browser is closed. Clear the cookie to log out.

When every provider for the requested model is in cooldown, the routed
endpoints answer `503` in their own error envelope with a `Retry-After` header
carrying the seconds until the *first* candidate comes back — waiting less than
that cannot succeed. The `503` for a config with no usable backend carries no
header, since waiting does not fix it.

`GET /health` is the cheap version of the same question, and is the one endpoint
the optional inbound `api_key` never guards:

```sh
curl -s localhost:41986/health | jq
{"status":"ok","providers":{"groq":"ok","openrouter":"cooldown","claude-cli":"auth"}}
```

The status code is **always `200`** while chicco is running — it is a liveness
probe, and the Helm chart wires readiness to it too, so failing it during a
provider outage would restart a proxy that is working fine. Read `status`
instead: `degraded` means every provider is greyed out or in cooldown right now,
so the next request will likely 503. Per provider: `ok`, `auth` (key rejected /
tool logged out), `down` (unreachable, or a CLI whose binary isn't installed),
`cooldown` (rate-limited, will come back), `unknown` (not probed yet).
