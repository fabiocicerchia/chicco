# chicco — a local OpenAI- and Anthropic-compatible rotation proxy.
#
# Every verb this repo exposes lives here; `make` on its own prints them,
# grouped, straight out of the `##` comments below.

BIN     := chicco
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := help
# help is pure output; the recipe echo would only be noise.
.SILENT: help

##@ General

.PHONY: help
help: ## Show this help
	awk 'BEGIN {FS = ":.*## "} \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } \
		/^[a-zA-Z_0-9-]+:.*## / { printf "  \033[36m%-13s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)

.PHONY: setup
setup: ## Install the pre-commit hook
	pre-commit install

##@ Build

.PHONY: build
build: ## Compile the binary into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/chicco

.PHONY: install
install: ## go install into GOBIN
	go install -ldflags "$(LDFLAGS)" ./cmd/chicco

.PHONY: run
run: ## Run chicco (pass args with ARGS="...")
	go run ./cmd/chicco $(ARGS)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist

##@ Quality

.PHONY: lint
lint: ## Run golangci-lint (falls back to go vet)
	@golangci-lint run ./... 2>/dev/null || go vet ./...

# -race, always: the rotator, the event logs and the dashboard all read the
# same state from different goroutines, so a green run without it proves less
# than it looks like it does.
.PHONY: test
test: ## Run tests with the race detector
	go test -race ./...

##@ Release

.PHONY: docker-build
docker-build: ## Build the chicco:latest image (see docs/DOCKER.md to run it)
	docker build --build-arg VERSION=$(VERSION) -t chicco .

.PHONY: snapshot
snapshot: ## Build a local unpublished release with GoReleaser
	goreleaser release --snapshot --clean
