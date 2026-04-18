SHELL := /bin/bash
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.cliVersion=$(VERSION)

# Sub-modules managed by go.work
WORKSPACE_MODULES := sdk sdkx examples/voice-pipeline

.PHONY: help
help:
	@echo "FlowCraft"
	@echo ""
	@echo "  make vet       Run go vet on all modules"
	@echo "  make test      Run tests on all modules"
	@echo "  make fmt       Run gofmt on all modules"
	@echo "  make tidy      Run go mod tidy on all modules"
	@echo "  make ci        vet + test"
	@echo "  make build     Build flowcraft binary"

.PHONY: vet
vet:
	@for m in $(WORKSPACE_MODULES); do echo "==> vet $$m"; ( cd $$m && go vet ./... ); done
	@echo "==> vet root"; GOWORK=off go vet ./...

.PHONY: test
test:
	@for m in $(WORKSPACE_MODULES); do echo "==> test $$m"; ( cd $$m && go test ./... -count=1 ); done
	@echo "==> test root"; GOWORK=off go test ./... -count=1

.PHONY: fmt
fmt:
	@for m in $(WORKSPACE_MODULES); do echo "==> fmt $$m"; ( cd $$m && go fmt ./... ); done
	@echo "==> fmt root"; GOWORK=off go fmt ./...

.PHONY: tidy
tidy:
	@for m in $(WORKSPACE_MODULES); do echo "==> tidy $$m"; ( cd $$m && go mod tidy ); done
	@echo "==> tidy root"; GOWORK=off go mod tidy

.PHONY: build
build:
	@echo "==> build flowcraft ($(VERSION))"; GOWORK=off go build -ldflags '$(LDFLAGS)' -o bin/flowcraft ./cmd/flowcraft

.PHONY: ci
ci: vet test
