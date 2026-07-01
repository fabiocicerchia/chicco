# Local development

## Requirements

| Tool | Needed for | Notes |
|------|-----------|-------|
| **Go 1.25+** | everything | the only hard requirement (`go.mod` pins `go 1.25.0`) |
| **git** | version control | |
| **make** | the `make` shortcuts below | optional — every target is a plain `go` command you can run directly |

Everything else is optional and only needed for a specific task:

| Tool | Used by | Install |
|------|---------|---------|
| [golangci-lint](https://golangci-lint.run) v2.12.2 | `make lint` | `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2` (or `brew install golangci-lint`) — without it `make lint` falls back to `go vet` |
| [GoReleaser](https://goreleaser.com) v2.16.0 | `make snapshot` | `go install github.com/goreleaser/goreleaser/v2@v2.16.0` (or `brew install goreleaser`) |
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln) v1.1.4 | vuln scan (CI) | `go install golang.org/x/vuln/cmd/govulncheck@v1.1.4` |
| [gitleaks](https://github.com/gitleaks/gitleaks) 8.30.1 | secret scan (CI) | `brew install gitleaks` or grab a release binary |
| [vhs](https://github.com/charmbracelet/vhs) + `ttyd` + `ffmpeg` | regenerating `docs/demo.gif` | `brew install vhs` (pulls ttyd + ffmpeg) |

## Make commands

Run `make help` to list these at any time.

| Command | What it does | Equivalent |
|---------|--------------|------------|
| `make build` | Compile the binary into `./bin/chicco` (version stamped from `git describe`) | `go build -o bin/chicco ./cmd/chicco` |
| `make run` | Run chicco; pass flags with `ARGS="..."` | `go run ./cmd/chicco` |
| `make test` | Run the test suite with the race detector | `go test -race ./...` |
| `make lint` | Run golangci-lint (falls back to `go vet` if it isn't installed) | `golangci-lint run ./...` |
| `make install` | Install `chicco` into your `GOBIN` | `go install ./cmd/chicco` |
| `make snapshot` | Build a local, unpublished release (binaries + archives + SBOMs in `dist/`) | `goreleaser release --snapshot --clean` |
| `make clean` | Remove `bin/` and `dist/` | |
| `make help` | List the targets above | |

## Common workflows

**Build and run against your config:**

```sh
make build
./bin/chicco -config chicco.yaml
# or in one step:
make run ARGS="-config chicco.yaml -headless"
```

**Before opening a PR** (mirrors CI):

```sh
make test
make lint
govulncheck ./...          # optional: matches the CI vuln job
```

**Test a full release locally** without publishing:

```sh
make snapshot              # writes binaries, archives, checksums, SBOMs to dist/
```

Note: the real release also cosign-signs the checksums via GitHub OIDC, which
only runs in CI (`.github/workflows/release.yml`) — `make snapshot` skips it.

**Regenerate the dashboard demo GIF:**

```sh
go build -o "$(go env GOPATH)/bin/chicco" ./cmd/chicco   # put chicco on PATH
vhs docs/demo.tape                                        # -> docs/demo.gif
```

The tape uses the offline demo config `docs/demo.chicco.yaml` (CLI providers, no
keys, no network).

## CI

`.github/workflows/ci.yml` runs on every push and PR: `test` (build + vet +
race tests), `lint` (golangci-lint), `vuln` (govulncheck), and `secrets`
(gitleaks over the full history). `release.yml` runs GoReleaser on a `v*` tag.
