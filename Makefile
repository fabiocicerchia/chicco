BIN     := chicco
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: build
build: ## Compile the binary into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BIN) .

.PHONY: run
run: ## Run chicco (pass args with ARGS="...")
	go run . $(ARGS)

.PHONY: test
test: ## Run tests with the race detector
	go test -race ./...

.PHONY: lint
lint: ## Run golangci-lint (falls back to go vet)
	@golangci-lint run ./... 2>/dev/null || go vet ./...

.PHONY: install
install: ## go install into GOBIN
	go install -ldflags "$(LDFLAGS)" .

.PHONY: snapshot
snapshot: ## Build a local unpublished release with GoReleaser
	goreleaser release --snapshot --clean

.PHONY: clean
clean:
	rm -rf bin dist

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk -F':.*?## ' '{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
