# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Local OpenAI-compatible rotation proxy: one `/v1/chat/completions` endpoint
  that round-robins across free-tier providers and fails over on quota/auth
  errors.
- HTTP and CLI-backed (`kind: cli`) providers, configured entirely in
  `chicco.yaml`.
- Live Bubble Tea dashboard with per-provider usage bars and a rolling log
  pane; `t` tests every configured model.
- Boot + periodic health probes that grey dead/unauthorized providers.
- Token accounting persisted to a JSON state file across restarts, with
  daily/monthly usage windows.
- Cross-platform release binaries (linux/darwin/windows, amd64/arm64) with an
  SBOM and keyless cosign signatures.
- One-line installer (`install.sh`).
- `GET /v1/models` endpoint listing `chicco:auto` plus the virtual models
  defined in `chicco.yaml`.
- Optional inbound `api_key` shared secret: when set, every endpoint except
  `/health` requires `Authorization: Bearer <key>` (constant-time compared).
- `-check` flag: statically validate the config (bad YAML, `kind: cli` without
  `command`, unknown `kind`/`output`/model `strategy`, duplicate names, …) and
  exit, without binding a port — safe for CI / pre-commit.
- Per-model load-balancing `strategy` (`order` default, `round_robin`, `random`,
  `weighted`) set on each virtual model in `models:`, with a per-provider
  `weight`, to spread load instead of always draining the top backend. A
  request that doesn't match a virtual model always uses plain config order.

[Unreleased]: https://github.com/fabiocicerchia/chicco/commits/main
