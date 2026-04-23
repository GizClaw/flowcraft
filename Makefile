# Pin bash so the GO_FOREACH macro's `set -e; for ... done` semantics are
# stable across hosts (default sh on some macOS setups treats `set -e`
# inside compound statements differently).
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work — `go vet ./...` and friends work as-is.
# sdk + sdkx + voice are the tightly-coupled core that needs atomic
# in-tree edits (sdkx imports sdk packages that may not yet exist in
# any released sdk version; voice depends on the same sdk source).
MODULES_WORK := sdk sdkx voice

# Modules intentionally outside go.work — they pin sdk/sdkx via go.mod
# require directives and run with GOWORK=off so the pin is honoured.
#
#  - bench: heavy eval datasets/CLIs we don't want polluting the workspace
#  - examples/voice-pipeline: pinned to sdk v0.1.12 + sdkx v0.1.14 so
#    external consumers see a real reproducible example
MODULES_OFFWORK := bench examples/voice-pipeline

ALL_MODULES := $(MODULES_WORK) $(MODULES_OFFWORK)

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
	@echo "  make vet       Run go vet on all modules (incl. bench via GOWORK=off)"
	@echo "  make test      Run tests on all modules (excl. Go benchmarks)"
	@echo "  make fmt       Run gofmt on all modules"
	@echo "  make tidy      Run go mod tidy on all modules"
	@echo "  make ci        vet + test"
	@echo ""
	@echo "Bench is in CI for vet+test only. Long-running eval CLIs"
	@echo "(bench/locomo/cmd/eval, history-compression/cmd/eval) are main"
	@echo "packages and are not invoked by 'go test ./...'."

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
