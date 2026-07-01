# Contributing to chicco

Thanks for helping out. chicco is a small, single-package Go tool — keep changes
that way.

## Development

```sh
git clone https://github.com/fabiocicerchia/chicco
cd chicco
make build      # -> ./bin/chicco
make test       # go test -race ./...
make lint       # golangci-lint (or: go vet ./...)
```

See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for every make target and the
full list of (mostly optional) tooling.

Adding a provider is usually a `chicco.yaml` entry, not code — see the README's
provider tables first.

## Pull requests

- Keep the diff focused; one logical change per PR.
- `make test` and `make lint` must pass. Add a test when you change non-trivial
  logic (this repo already has table tests next to each source file).
- Use [Conventional Commits](https://www.conventionalcommits.org/) for messages
  (`feat:`, `fix:`, `docs:`, …) — the release notes are generated from them.
- Note user-facing changes in `CHANGELOG.md` under `Unreleased`.

## Reporting bugs

Open an issue with the chicco version (`chicco -version`), your OS/arch, and the
relevant `chicco.yaml` (redact keys). For security issues, see
[SECURITY.md](SECURITY.md) instead.

By contributing you agree your work is licensed under the [MIT License](LICENSE).
