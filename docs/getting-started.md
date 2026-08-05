# Getting started

## Pointing an agent at chicco

chicco is a plain OpenAI- and Anthropic-compatible endpoint, so any client that
lets you set a base URL works — point it at `http://127.0.0.1:41986/v1` (no API
key needed).
The [`examples/`](../examples/) folder has ready configs for **OpenCode**,
**Continue**, **Aider**, **Headroom** (context compression in front of chicco),
and the raw OpenAI SDK / `curl`.

Note: chicco overrides the request's `model` field with its rotation pick, so the
model name a client sends is arbitrary.

## Install

**One-liner** (Linux / macOS, amd64 / arm64) — downloads the latest release,
verifies its SHA-256, and installs the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/fabiocicerchia/chicco/main/install.sh | sh
```

Set `BINDIR` to choose the install directory, or `CHICCO_VERSION` to pin a tag.

**Go:**

```sh
go install github.com/fabiocicerchia/chicco/cmd/chicco@latest
```

**Manual:** grab an archive from the [releases page](https://github.com/fabiocicerchia/chicco/releases/latest).
Every release ships an SPDX SBOM and a keyless [cosign](https://github.com/sigstore/cosign)
signature over `checksums.txt`; verify it before trusting a download:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/fabiocicerchia/chicco' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

**Docker:** pull the multi-arch image (keyless-signed with cosign, same
posture as the checksums above) published by every tagged release, or build
it yourself from the repo — see [docs/DOCKER.md](DOCKER.md) for the full
walkthrough (state persistence, env-var keys, signature verification,
docker-compose):

```sh
docker pull ghcr.io/fabiocicerchia/chicco:latest
docker run --rm -p 41986:41986 \
  -v "$(pwd)/chicco.yaml:/etc/chicco/chicco.yaml:ro" \
  --env-file .env \
  ghcr.io/fabiocicerchia/chicco:latest
```
