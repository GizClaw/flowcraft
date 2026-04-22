SHELL := /bin/bash
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.cliVersion=$(VERSION)

OPENAPI_SPEC := internal/api/openapi.yaml

# Independent Go modules in this repo.
# Each module is treated as if checked out separately (no go.work);
# sdkx / plugin / examples consume sdk via the published version pinned in their go.mod.
GO_MODULES := . sdk sdkx plugin examples/voice-pipeline

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
	@echo "  make generate         Regenerate all code from openapi.yaml"
	@echo "  make gen-go           Regenerate Go server stubs (ogen)"
	@echo "  make gen-web          Regenerate Web TypeScript types (openapi-typescript)"
	@echo "  make verify-generated Verify generated code is in sync with openapi.yaml"

.PHONY: vet
vet:
	@for m in $(GO_MODULES); do echo "==> vet $$m"; ( cd $$m && go vet ./... ); done

.PHONY: test
test:
	@for m in $(GO_MODULES); do echo "==> test $$m"; ( cd $$m && go test ./... -count=1 ); done

.PHONY: fmt
fmt:
	@for m in $(GO_MODULES); do echo "==> fmt $$m"; ( cd $$m && go fmt ./... ); done

.PHONY: tidy
tidy:
	@for m in $(GO_MODULES); do echo "==> tidy $$m"; ( cd $$m && go mod tidy ); done

.PHONY: build
build:
	@echo "==> build flowcraft ($(VERSION))"; go build -ldflags '$(LDFLAGS)' -o bin/flowcraft ./cmd/flowcraft

.PHONY: generate
generate: gen-go gen-web

.PHONY: gen-go
gen-go:
	@echo "==> generate Go server (ogen)"
	cd internal/api && go generate ./...

.PHONY: gen-web
gen-web:
	@echo "==> generate Web TypeScript types (openapi-typescript)"
	cd web && npx openapi-typescript ../$(OPENAPI_SPEC) -o src/api/schema.d.ts

.PHONY: verify-generated
verify-generated: generate
	@echo "==> verify generated code matches $(OPENAPI_SPEC)"
	@if ! git diff --quiet -- internal/api/oas web/src/api/schema.d.ts; then \
		echo "ERROR: generated code is out of sync with $(OPENAPI_SPEC)."; \
		echo "Run 'make generate' and commit the result."; \
		git --no-pager diff --stat -- internal/api/oas web/src/api/schema.d.ts; \
		exit 1; \
	fi
	@echo "OK: generated code is in sync."

.PHONY: ci
ci: vet test
