# Running chicco in Docker

The image ships no config and no keys — `chicco.yaml` holds provider secrets,
so it's always mounted in at run time, never baked into a layer. Build once,
then run against whatever config/`.env` pair you'd use outside Docker.

## Pull the published image

Every tagged release (`.github/workflows/release.yml`) builds and pushes a
multi-arch (`linux/amd64`, `linux/arm64`) image to GHCR, keyless-signed with
cosign the same way `checksums.txt` is signed for the binary releases:

```sh
docker pull ghcr.io/fabiocicerchia/chicco:latest
# or a specific version:
docker pull ghcr.io/fabiocicerchia/chicco:v1.2.3
```

Verify the signature before trusting a pulled image (needs
[cosign](https://github.com/sigstore/cosign)):

```sh
cosign verify ghcr.io/fabiocicerchia/chicco:v1.2.3 \
  --certificate-identity-regexp 'https://github.com/fabiocicerchia/chicco' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Skip to [Run, mounting your config](#run-mounting-your-config) below — same
`docker run` either way, just swap `chicco` for the `ghcr.io/...` image name.
Or build your own from source:

## Build

```sh
docker build -t chicco .
```

Pin a version string into `chicco -version` output with `--build-arg`:

```sh
docker build --build-arg VERSION=$(git describe --tags --always) -t chicco .
```

## Run, mounting your config

Same `chicco.yaml` / `.env` split as the non-Docker [Quick start](../README.md#quick-start) —
`api_key: ${GROQ_API_KEY}` style references are expanded from the
container's environment, so pass your `.env` straight through:

```sh
docker run --rm -p 41986:41986 \
  -v "$(pwd)/chicco.yaml:/etc/chicco/chicco.yaml:ro" \
  --env-file .env \
  chicco
```

Then point any OpenAI client at `http://127.0.0.1:41986/v1`, same as a
non-Docker run. The mount is read-only (`:ro`) since chicco never writes to
its own config.

## Persisting token usage across restarts

By default the container's `-state` path (`/var/lib/chicco/chicco-state.json`,
set by the image's `CMD`) lives in the container's writable layer — usage
counters reset if the container is removed. Mount a volume to keep them:

```sh
docker run --rm -p 41986:41986 \
  -v "$(pwd)/chicco.yaml:/etc/chicco/chicco.yaml:ro" \
  -v chicco-state:/var/lib/chicco \
  --env-file .env \
  chicco
```

(`chicco-state` here is a named volume — `docker volume create chicco-state`
first, or let `docker run` create it implicitly.)

## Reloading config on a key rotation or provider edit

chicco reloads `chicco.yaml` on `SIGHUP` without a restart (see the README's
"Reloading the config" section) — `docker kill --signal=HUP <container>`
after editing the mounted file works the same way.

## The TUI dashboard vs. headless logging

chicco already auto-detects a non-TTY stdout and falls back to headless mode
(plain stderr logging) — the default `docker run` (no `-it`) gets this for
free, so `docker logs <container>` shows the same log lines the TUI's log
pane would. To get the live TUI dashboard instead, allocate a pty:

```sh
docker run --rm -it -p 41986:41986 \
  -v "$(pwd)/chicco.yaml:/etc/chicco/chicco.yaml:ro" \
  --env-file .env \
  chicco
```

Either way, `GET /v1/status` and the web dashboard at `/dashboard` work
regardless of TTY — see the README's "Endpoints" table.

## docker-compose

```yaml
services:
  chicco:
    image: ghcr.io/fabiocicerchia/chicco:latest   # or `build: .` to build from source
    ports:
      - "41986:41986"
    volumes:
      - ./chicco.yaml:/etc/chicco/chicco.yaml:ro
      - chicco-state:/var/lib/chicco
    env_file: .env
    restart: unless-stopped

volumes:
  chicco-state:
```

## Notes

- The image runs as a non-root user (`chicco`), so a bind-mounted (not named-volume)
  state directory needs to be writable by that user's UID on the host — a
  named volume (as above) avoids the issue entirely, since Docker manages its
  ownership.
- `ca-certificates` is installed in the runtime image — chicco calls HTTPS
  provider APIs directly, so this is required, not optional.
- First publish to a new GHCR package can land **private** by default (a
  GitHub quirk, not something the workflow controls) — if `docker pull` gets
  a 403/denied on a public repo's first release, flip the package's
  visibility to public once under the repo's Packages settings; every
  release after that stays public.
- Multi-arch images are built with `docker buildx` (QEMU + native
  cross-compilation via `--platform=$BUILDPLATFORM`/`GOOS`/`GOARCH` in the
  Dockerfile, so the Go build itself isn't emulated) — a plain `docker build`
  needs the buildx component; see
  [Docker's install docs](https://docs.docker.com/build/architecture/#buildx)
  if `docker build` errors with "buildx component is missing".
