SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULES := sdk sdkx examples/voice-pipeline

.PHONY: help
help:
	@echo "FlowCraft"
	@echo ""
	@echo "  make vet       Run go vet on all modules"
	@echo "  make test      Run tests on all modules"
	@echo "  make fmt       Run gofmt on all modules"
	@echo "  make tidy      Run go mod tidy on all modules"
	@echo "  make ci        vet + test"

.PHONY: vet
vet:
	@for m in $(MODULES); do echo "==> vet $$m"; ( cd $$m && go vet ./... ); done

.PHONY: test
test:
	@for m in $(MODULES); do echo "==> test $$m"; ( cd $$m && go test ./... -count=1 ); done

.PHONY: fmt
fmt:
	@for m in $(MODULES); do echo "==> fmt $$m"; ( cd $$m && go fmt ./... ); done

.PHONY: tidy
tidy:
	@for m in $(MODULES); do echo "==> tidy $$m"; ( cd $$m && go mod tidy ); done

.PHONY: ci
ci: vet test
