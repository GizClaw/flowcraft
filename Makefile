# Pin bash so the GO_FOREACH macro's `set -e; for ... done` semantics are
# stable across hosts (default sh on some macOS setups treats `set -e`
# inside compound statements differently).
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work — `go vet ./...` and friends work as-is.
# sdk + sdkx are the tightly-coupled core that needs atomic in-tree edits
# (sdkx imports sdk).
MODULES_WORK := core driver sdk memory sdkx examples/forge

# Modules gated by CI's gofmt -s + golangci-lint lanes.
MODULES_LINT := core driver sdk memory memory/eval sdkx

# `make fmt` mirrors the CI gofmt -s gate; memory/eval is included here
# even though it is not part of MODULES_WORK (vet/test run).
ALL_MODULES := $(MODULES_WORK) memory/eval

# `set -e` inside the for-loop body so a failure in any submodule stops the
# loop. The previous form (` ( cd $$m && ... ) `) silently swallowed errors
# from the subshell because the for-body's last command was the `done`, not
# the failing subshell — make then saw exit 0 from the loop and reported the
# whole target green.
GO_FOREACH = set -e; for m in $(1); do echo "==> $(2) $$m"; ( cd $$m && $(3) ); done

.PHONY: help
help:
	@echo "FlowCraft"
	@echo ""
	@echo "  make vet         Run go vet on all modules"
	@echo "  make test        Run tests on all modules (excl. Go benchmarks)"
	@echo "  make fmt         Run gofmt -s on all modules"
	@echo "  make lint        Run golangci-lint on CI-gated modules"
	@echo "  make tidy        Run go mod tidy on all modules"
	@echo "  make ci          vet + test"
	@echo "  make release-check  Test release tooling, validate changesets, and"
	@echo "                      verify the pending module release plan."
	@echo "  make release-plan   Print the pending module release plan as JSON."
	@echo "  make release-preflight  Verify planned modules stay tidy when released together."
	@echo "  make release-preflight-write  Apply preflight tidy results to go.mod/go.sum."
	@echo "  make release-changelog  Aggregate pending changesets into CHANGELOG.md."
	@echo ""
	@echo "  make eval              Hermetic memory retrieval eval (memory/eval module)."
	@echo "  make eval-smoke        Compatibility alias for the hermetic memory eval."
	@echo "  make test-quality      Alias of 'make eval' kept for compatibility with"
	@echo "                         the pre-eval/ migration entry point."
	@echo ""
	@echo "The memory eval uses fixed fixtures and requires no network or credentials."

.PHONY: vet
vet:
	@$(call GO_FOREACH,$(MODULES_WORK),vet,go vet ./...)

.PHONY: test
test:
	@$(call GO_FOREACH,$(MODULES_WORK),test,go test ./... -count=1)

.PHONY: fmt
fmt:
	@$(call GO_FOREACH,$(ALL_MODULES),fmt,gofmt -s -w .)

.PHONY: lint
lint:
	@$(call GO_FOREACH,$(MODULES_LINT),lint,golangci-lint run --timeout 5m ./...)

.PHONY: tidy
tidy:
	@$(call GO_FOREACH,$(MODULES_WORK),tidy,go mod tidy)

.PHONY: ci
ci: vet test

.PHONY: release-check
release-check:
	@cd tools/releasegate && GOWORK=off go test -count=1 ./...
	@if [[ -n "$(BASE)" ]]; then \
		cd tools/releasegate && GOWORK=off go run . validate --repo ../.. --base "$(BASE)"; \
	else \
		cd tools/releasegate && GOWORK=off go run . validate --repo ../..; \
	fi
	@cd tools/releasegate && GOWORK=off go run . plan --repo ../..

.PHONY: release-plan
release-plan:
	@cd tools/releasegate && GOWORK=off go run . plan --repo ../.. --json

.PHONY: release-preflight
release-preflight:
	@cd tools/releasegate && GOWORK=off go run . preflight --repo ../..

.PHONY: release-preflight-write
release-preflight-write:
	@cd tools/releasegate && GOWORK=off go run . preflight --repo ../.. --write

.PHONY: release-changelog
release-changelog:
	@cd tools/releasegate && GOWORK=off go run . changelog --repo ../.. --write

# Credential-free retrieval quality evaluation over real memory components.
.PHONY: eval
eval:
	@cd memory/eval && go test ./... -count=1 -v

# Compatibility target retained after removal of the old top-level eval module.
.PHONY: eval-smoke
eval-smoke: eval

# Backwards-compat alias for the former quality target.
.PHONY: test-quality
test-quality: eval
