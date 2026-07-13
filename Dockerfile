# syntax=docker/dockerfile:1

# Build stage — compiles a static binary, matching Makefile's `build` target
# (CGO disabled, -s -w -X main.version, see the Makefile for the non-Docker
# equivalent). --platform=$BUILDPLATFORM + GOOS/GOARCH cross-compiles Go
# natively on the runner's own arch for a multi-platform buildx build, instead
# of emulating the whole compile under QEMU for the non-native target.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine@sha256:523c3effe300580ed375e43f43b1c9b091b68e935a7c3a92bfcc4e7ed55b18c2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/chicco ./cmd/chicco

# Runtime stage — no source, no build tools, no baked-in config (chicco.yaml
# holds provider keys, so it's mounted at run time — see examples/README.md
# and docs/DOCKER.md).
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk add --no-cache ca-certificates && \
    addgroup -S chicco && adduser -S chicco -G chicco && \
    mkdir -p /var/lib/chicco && chown chicco:chicco /var/lib/chicco
COPY --from=build /out/chicco /usr/local/bin/chicco
USER chicco
EXPOSE 41986
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:41986/health || exit 1
ENTRYPOINT ["/usr/local/bin/chicco"]
# Defaults assume the conventional mount points from docs/DOCKER.md; override
# by passing different flags after the image name (they replace this CMD).
CMD ["-config", "/etc/chicco/chicco.yaml", "-state", "/var/lib/chicco/chicco-state.json"]
