# Pin bash so the GO_FOREACH macro's `set -e; for ... done` semantics are
# stable across hosts (default sh on some macOS setups treats `set -e`
# inside compound statements differently).
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work — `go vet ./...` and friends work as-is.
# sdk + memory + sdkx + voice are the tightly-coupled core that needs atomic
# in-tree edits (memory depends on sdk, sdk compatibility shims point at
# memory, sdkx imports both, and voice depends on the same sdk source).
MODULES_WORK := sdk memory sdkx vessel voice cmd/vesseld eval

# Modules intentionally outside go.work — they pin sdk/sdkx via go.mod
# require directives and run with GOWORK=off so the pin is honoured.
#
#  - examples/voice-pipeline: pinned to sdk v0.1.12 + sdkx v0.1.14 so
#    external consumers see a real reproducible example
#  - tests/conformance: manual provider conformance suites. The module
#    runs outside go.work so its go.mod replace directives explicitly
#    select the in-tree sdk/memory/sdkx APIs under test. Tests self-skip
#    without credentials so `make test` runs them as a compile check;
#    `make conformance` is the documented entry point when a .env is
#    in place.
#  - tests/e2e/vesseld: black-box subprocess tests for the vesseld
#    binary. Tagged with `//go:build e2e` so `make test`'s default
#    sweep is just a compile check; the credentialed / build-tagged
#    lane runs via `make test-e2e`.
MODULES_OFFWORK := examples/voice-pipeline tests/conformance tests/quality/vessel tests/e2e/vesseld tests/e2e/retrieval

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
	@echo "  make vet         Run go vet on all modules"
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
	@echo "  make eval              AI-quality eval suites (eval/, all sub-packages)."
	@echo "                         Hermetic synthetic lanes only; LLM-backed lanes"
	@echo "                         self-skip without credentials."
	@echo "  make eval-smoke        End-to-end smoke: run the LoCoMo eval CLI on the"
	@echo "                         bundled synthetic dataset and write a report."
	@echo "  make test-quality      Alias of 'make eval' kept for compatibility with"
	@echo "                         the pre-eval/ migration entry point."
	@echo "  make test-e2e          Black-box e2e suite for vesseld"
	@echo "                         (tests/e2e/vesseld, //go:build e2e)."
	@echo "                         Builds the vesseld binary and runs it"
	@echo "                         against an in-process mock OpenAI server;"
	@echo "                         no network or API key required."
	@echo "  make ci-e2e            ci + test-e2e."
	@echo ""
	@echo "Eval suites under eval/ run vet+test in CI; the long-running"
	@echo "unified CLI lives at eval/cmd/eval (run as 'go run ./cmd/eval'"
	@echo "from inside eval/) and is a main package, not invoked by"
	@echo "'go test ./...'."

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

# Provider conformance: runs every suite under tests/conformance with
# GOWORK=off so the module's explicit replace directives define which
# in-tree sdk/memory/sdkx APIs are under test. Without credentials the
# individual tests Skip cleanly, so this also doubles as a compile check.
.PHONY: test-conformance
test-conformance:
	@cd tests/conformance && GOWORK=off go test -count=1 ./...

# AI-quality evaluation suites under eval/. The default lane is hermetic
# (no credentials, synthetic LoCoMo fixture, BM25-only knowledge corpus);
# integration lanes self-skip without credentials.
.PHONY: eval
eval:
	@cd eval && go test ./... -count=1

# eval-smoke is the end-to-end "does the unified CLI still link?"
# check. It runs `eval locomo run` against the bundled synthetic
# dataset with no LLM wired up, so a clean environment can run it as
# part of CI / pre-push.
.PHONY: eval-smoke
eval-smoke:
	@cd eval && go run ./cmd/eval locomo run --dataset synthetic --out /tmp/eval-locomo-synthetic.json
	@echo "wrote /tmp/eval-locomo-synthetic.json"

# Backwards-compat alias for the pre-eval/ migration entry point. The old
# target only ran the //go:build integration lane of tests/quality/knowledge;
# the post-migration `make eval` covers more (LoCoMo, history, knowledge),
# but the integration lane still requires KNOWLEDGE_EVAL_EMBEDDER (e.g.
# `qwen:text-embedding-v4`) plus the matching FLOWCRAFT_<ALIAS> JSON to
# actually do work.
.PHONY: test-quality
test-quality:
	@cd eval/knowledge && GOWORK=off go test -tags=integration -count=1 ./...
