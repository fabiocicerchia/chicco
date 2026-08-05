# Endpoints

## Endpoints

| Method | Path                    | Purpose                                          |
|--------|-------------------------|--------------------------------------------------|
| `POST` | `/v1/chat/completions`  | rotated, proxied chat completion (OpenAI-shaped) |
| `POST` | `/v1/messages`          | rotated, proxied chat completion (Anthropic-shaped) |
| `POST` | `/v1/embeddings`        | rotated, proxied embeddings (OpenAI-shaped); HTTP providers only |
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
