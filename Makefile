# Pin bash so the GO_FOREACH macro's `set -e; for ... done` semantics are
# stable across hosts (default sh on some macOS setups treats `set -e`
# inside compound statements differently).
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work — `go vet ./...` and friends work as-is.
BACKEND_MODULES := $(patsubst %/go.mod,%,$(wildcard backends/*/go.mod backends/*/*/go.mod))
DRIVER_MODULES := $(patsubst %/go.mod,%,$(wildcard driver/*/go.mod))

MODULES_WORK := core craft $(BACKEND_MODULES) $(DRIVER_MODULES) examples/forge examples/anvil

# Modules gated by CI's gofmt -s + golangci-lint lanes.
MODULES_LINT := core craft $(BACKEND_MODULES) $(DRIVER_MODULES)

# Modules a reader builds from their own go.mod and go.sum, without this
# workspace: every example, plus `craft` itself — a reader requires it
# directly, so its own pins have to stand on their own, and the workspace
# was hiding exactly that. They are checked with GOWORK=off (see
# `check-consumers`); the drivers get the same treatment per module in
# CI's test matrix.
#
# An exemption is a claim about today, and it is checked as one: a module
# listed here that starts building outside the workspace fails the
# target, so an exemption cannot outlive the release that made it
# obsolete. Empty since core v0.7.0 shipped the two symbols that kept
# craft and examples/anvil pinned below it (core/hooks and
# core/tool/mcp.WithServerLost); name a module here only with the reason
# it cannot build yet.
CONSUMER_EXEMPT :=
CONSUMER_MODULES := $(filter-out $(CONSUMER_EXEMPT),$(patsubst %/go.mod,%,$(wildcard examples/*/go.mod)) craft)

# `make fmt` mirrors the CI gofmt -s gate.
ALL_MODULES := $(MODULES_WORK)

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
	@echo "  make check-consumers  Build the examples outside the workspace (GOWORK=off),"
	@echo "                      plus craft itself; exemptions must still be exempt."
	@echo "  make ci          vet + test"
	@echo "  make release-check  Test release tooling, validate changesets, and"
	@echo "                      verify the pending module release plan."
	@echo "  make release-plan   Print the pending module release plan as JSON."
	@echo "  make release-preflight  Verify planned modules stay tidy when released together."
	@echo "  make release-preflight-write  Apply preflight tidy results to go.mod/go.sum."
	@echo "  make release-changelog  Aggregate pending changesets into CHANGELOG.md."

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

# Modules are installed with GOBIN pointed at a temp directory: `go
# install` writes whatever binary a module produces outside the tree
# (nothing is ever left behind next to the source), and it is a no-op for
# a library module like `craft` — where `go build -o dir/` refuses to run
# at all ("no main packages to build").
.PHONY: check-consumers
check-consumers:
	@set -e; out=$$(mktemp -d); trap 'rm -rf "$$out"' EXIT; \
	for m in $(CONSUMER_MODULES); do \
		echo "==> consumer build $$m"; \
		( cd $$m && GOWORK=off go mod tidy -diff && GOBIN="$$out" GOWORK=off go install ./... ); \
	done
	@set -e; out=$$(mktemp -d); trap 'rm -rf "$$out"' EXIT; \
	for m in $(CONSUMER_EXEMPT); do \
		echo "==> consumer exemption $$m"; \
		if log=$$(cd $$m && GOBIN="$$out" GOWORK=off go install ./... 2>&1); then \
			echo "error: $$m builds outside the workspace now;" \
				"add it to CONSUMER_MODULES and drop the exemption" >&2; \
			exit 1; \
		fi; \
		echo "$$log" | head -1 | sed 's/^/    /'; \
	done

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

# Live probes: the credentialed measurement lanes of the memory eval module.
# They need backends/memory/eval/.env (or a repo-root .env) and spend model
# calls, so they are opt-in rather than part of `make ci`.
memory-eval-probes:
	cd backends/memory/eval && MEMORY_EVAL_LIVE=1 go test ./cmd/memory-eval/ -count=1 -v \
		-run 'Test(EvidenceFidelity|RetrievalBudgetSweep|PackingRedundancy|IngestTagsDatasetTurns|AnswerContextSize|RejudgeStoredAnswersWithLenientJudge|ConfiguredModelsExistInCatalog|DeriveConcurrencyLiveMatchesSequential|CoIngestedConversationsDoNotChangeRetrieval|ExtractionModelAB)' \
		-timeout 60m
