# Pin bash so the GO_FOREACH macro's `set -e; for ... done` semantics are
# stable across hosts (default sh on some macOS setups treats `set -e`
# inside compound statements differently).
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work — `go vet ./...` and friends work as-is.
# sdk + sdkx + voice are the tightly-coupled core that needs atomic
# in-tree edits (sdkx imports sdk packages that may not yet exist in
# any released sdk version; voice depends on the same sdk source).
MODULES_WORK := sdk sdkx vessel voice cmd/vesseld

# Modules intentionally outside go.work — they pin sdk/sdkx via go.mod
# require directives and run with GOWORK=off so the pin is honoured.
#
#  - bench: heavy eval datasets/CLIs we don't want polluting the workspace
#  - examples/voice-pipeline: pinned to sdk v0.1.12 + sdkx v0.1.14 so
#    external consumers see a real reproducible example
#  - tests/conformance: manual provider conformance suites; pinned to
#    released sdk/sdkx so we test the exact bytes consumers can pull.
#    Tests self-skip without credentials so `make test` runs them as a
#    compile check; `make conformance` is the documented entry point
#    when a .env is in place.
#  - tests/quality/knowledge: in-process retrieval-quality regression
#    suite for sdk/knowledge. Pins sdk/sdkx via go.mod so quality
#    drifts are evaluated against the released bytes consumers run,
#    and the 100-doc corpus stays out of every sdk patch tag. The
#    default lane (BM25 only) needs no credentials; the `integration`
#    lane requires `EMBEDDING_*` env vars and is opt-in via a build
#    tag, so `make test` exercises the compile path here too.
#  - tests/e2e/vesseld: black-box subprocess tests for the vesseld
#    binary. Tagged with `//go:build e2e` so `make test`'s default
#    sweep is just a compile check; the credentialed / build-tagged
#    lane runs via `make test-e2e`.
MODULES_OFFWORK := bench examples/voice-pipeline tests/conformance tests/quality/knowledge tests/quality/vessel tests/e2e/vesseld tests/e2e/retrieval

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
	@echo "  make vet         Run go vet on all modules (incl. bench via GOWORK=off)"
	@echo "  make test        Run tests on all modules (excl. Go benchmarks)"
	@echo "  make fmt         Run gofmt on all modules"
	@echo "  make tidy        Run go mod tidy on all modules"
	@echo "  make ci          vet + test"
	@echo ""
	@echo "Test suites under tests/ (default 'make test' already runs them"
	@echo "in compile-check / no-credential mode; the targets below are the"
	@echo "documented entry points for the credentialed / build-tagged lanes):"
	@echo ""
	@echo "  make test-conformance  Provider conformance suites (tests/conformance)."
	@echo "                         Needs a repo-root .env with provider credentials;"
	@echo "                         no env => suites self-skip and pass."
	@echo "  make test-quality      Retrieval-quality regression suite"
	@echo "                         (tests/quality/knowledge, integration lane)."
	@echo "                         Needs EMBEDDING_PROVIDER / EMBEDDING_API_KEY /"
	@echo "                         EMBEDDING_MODEL (or skips cleanly)."
	@echo "  make test-e2e          Black-box e2e suite for vesseld"
	@echo "                         (tests/e2e/vesseld, //go:build e2e)."
	@echo "                         Builds the vesseld binary and runs it"
	@echo "                         against an in-process mock OpenAI server;"
	@echo "                         no network or API key required."
	@echo "  make ci-e2e            ci + test-e2e."
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

# `make test-e2e` runs the build-tagged e2e suite against a freshly
# `go build`-ed vesseld binary. Each test spins up a subprocess
# bound to a per-test temp socket and a per-test mock OpenAI HTTP
# server, so the suite has no external dependencies (no API key,
# no network).
#
# Default `make test` excludes this lane because each test pays a
# ~1s build cost and a few seconds of subprocess setup; CI runs it
# explicitly via `make ci-e2e` (or by adding it to your local
# pre-push hook).
.PHONY: test-e2e
test-e2e:
	@cd tests/e2e/vesseld && GOWORK=off go test -tags=e2e -count=1 ./...
	@cd tests/e2e/retrieval && GOWORK=off go test -tags=e2e -count=1 -timeout 120s ./...

.PHONY: ci-e2e
ci-e2e: ci test-e2e

# Provider conformance: runs every suite under tests/conformance against
# the pinned sdk/sdkx release. Without credentials the individual tests
# Skip cleanly, so this also doubles as a "do the suites still compile
# against the released API?" check.
.PHONY: test-conformance
test-conformance:
	@cd tests/conformance && GOWORK=off go test -count=1 ./...

# Retrieval-quality regression suite. The default lane (BM25 only)
# is exercised by `make test` because tests/quality/knowledge is in
# MODULES_OFFWORK. This target opts into the //go:build integration
# lane that exercises the vector + hybrid lanes against a live
# embedding provider; tests self-skip when EMBEDDING_PROVIDER /
# EMBEDDING_API_KEY / EMBEDDING_MODEL are unset, so a credential-less
# run is still a no-op.
.PHONY: test-quality
test-quality:
	@cd tests/quality/knowledge && GOWORK=off go test -tags=integration -count=1 ./...
