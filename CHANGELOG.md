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

[Unreleased]: https://github.com/fabiocicerchia/chicco/commits/main
