SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work. These are tested with the workspace overlay so
# cross-module changes resolve to the monorepo checkout.
MODULES_WORK := sdk memory sdkx voice eval

# Modules intentionally outside go.work. They pin released module versions and
# run with GOWORK=off so the pins are honored.
MODULES_OFFWORK := examples/voice-pipeline

ALL_MODULES := $(MODULES_WORK) $(MODULES_OFFWORK)

GO_FOREACH = set -e; for m in $(1); do echo "==> $(2) $$m"; ( cd $$m && $(3) ); done

.PHONY: help
help:
	@echo "FlowCraft"
	@echo ""
	@echo "  make vet               Run go vet on maintained modules"
	@echo "  make test              Run tests on maintained modules"
	@echo "  make fmt               Run gofmt on maintained modules"
	@echo "  make tidy              Run go mod tidy on maintained modules"
	@echo "  make ci                vet + test"
	@echo ""
	@echo "Optional suites:"
	@echo "  make test-conformance  Provider conformance suites (tests/conformance)."
	@echo "                         No credentials => suites self-skip and pass."
	@echo "  make eval              Evaluation suites under eval/."

.PHONY: vet
vet:
	@$(call GO_FOREACH,$(MODULES_WORK),vet,go vet ./...)
	@$(call GO_FOREACH,$(MODULES_OFFWORK),vet (GOWORK=off),GOWORK=off go vet ./...)

.PHONY: test
test:
	@$(call GO_FOREACH,$(MODULES_WORK),test,go test ./... -count=1)
	@$(call GO_FOREACH,$(MODULES_OFFWORK),test (GOWORK=off),GOWORK=off go test ./... -count=1)

.PHONY: fmt
fmt:
	@$(call GO_FOREACH,$(ALL_MODULES),fmt,go fmt ./...)

.PHONY: tidy
tidy:
	@$(call GO_FOREACH,$(MODULES_WORK),tidy,go mod tidy)
	@$(call GO_FOREACH,$(MODULES_OFFWORK),tidy (GOWORK=off),GOWORK=off go mod tidy)

.PHONY: ci
ci: vet test

.PHONY: test-conformance
test-conformance:
	@cd tests/conformance && GOWORK=off go test -count=1 ./...

.PHONY: eval
eval:
	@cd eval && go test ./... -count=1
