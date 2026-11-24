# shellcheck disable=SC2046,SC2283,SC1036,SC1088,SC2034
# Make expands $(...) before the shell sees it: the unquoted expansions are
# argument lists (module names), and quoting them would pass one string.
#
# The same reading applies to the macro assignments below (`NAME = value` is
# make syntax, not shell) and to the `$$(...)` escaping in recipes: shellcheck
# parses this file as shell, so those three forms are reported and are not
# defects. The directives are disabled here rather than per line so a new
# recipe copying the pattern does not reintroduce the noise.
#
# Pin bash so the GO_FOREACH macro's `set -e; for ... done` semantics are
# stable across hosts (default sh on some macOS setups treats `set -e`
# inside compound statements differently).
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work — `go vet ./...` and friends work as-is.
#
# The inference implementations share one module: the protocol families (openai,
# anthropic), their three-stage pipeline, and the vendor adapters. The sandbox
# implementations share another, and the agent implementations a third — a new
# provider, backend or driver is a package beside them, never a new module.
# Spelled out rather than globbed: a wildcard that stops matching a consolidated
# module silently drops it from every target below.
INFERENCE_MODULES := inference
SANDBOX_MODULES := sandbox
AGENT_MODULES := agent
# The tool sources the platform ships — files, exec, skills, mcp — are one module for the same
# reason the others are: a new source is a package beside them, and core keeps the contract.
TOOL_MODULES := tool

MODULES_WORK := core $(SANDBOX_MODULES) $(INFERENCE_MODULES) $(AGENT_MODULES) $(TOOL_MODULES) examples/forge

# Modules gated by CI's gofmt -s + golangci-lint lanes.
MODULES_LINT := core $(SANDBOX_MODULES) $(INFERENCE_MODULES) $(AGENT_MODULES) $(TOOL_MODULES)

# `go mod tidy` resolves the core version a module pins against the module proxy.
# The three implementation modules pin a core that is only a commit on main so
# far, so `make release-preflight` is what tidies them, with a temporary local
# replace; every other module is tidied here.
MODULES_TIDY := $(filter-out $(SANDBOX_MODULES) $(INFERENCE_MODULES) $(AGENT_MODULES) $(TOOL_MODULES),$(MODULES_WORK))

# Modules a reader builds from their own go.mod and go.sum, without this
# workspace: every example. They are checked with GOWORK=off (see
# `check-consumers`); the drivers get the same treatment per module in
# CI's test matrix.
#
# An exemption is a claim about today, and it is checked as one: a module
# listed here that starts building outside the workspace fails the
# target, so an exemption cannot outlive the release that made it
# obsolete. Name a module here only with the reason it cannot build yet.
CONSUMER_EXEMPT :=
CONSUMER_MODULES := $(filter-out $(CONSUMER_EXEMPT),$(patsubst %/go.mod,%,$(wildcard examples/*/go.mod)))

# `make fmt` mirrors the CI gofmt -s gate.
ALL_MODULES := $(MODULES_WORK)

# A workspace member that is also a consumer pins released versions, so its vet
# and test run with GOWORK=off — the way a reader builds it. Inside the
# workspace it would be compiled against the working tree, and a core change
# the pin never sees would redden a module that does not depend on it.
# Splitting the set here rather than naming forge keeps the rule automatic when
# an example joins or leaves the workspace.
MODULES_TEST := $(filter-out $(CONSUMER_MODULES),$(MODULES_WORK))
MODULES_CONSUMER := $(filter-out $(MODULES_TEST),$(MODULES_WORK))

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
	@echo "  make vet         Run go vet on all modules (consumers outside the workspace)"
	@echo "  make test        Run tests on all modules (excl. Go benchmarks; consumers"
	@echo "                   outside the workspace)"
	@echo "  make fmt         Run gofmt -s on all modules"
	@echo "  make lint        Run golangci-lint on CI-gated modules"
	@echo "  make tidy        Run go mod tidy on all modules"
	@echo "  make bench       Run benchmarks for BENCH_MODULES into .bench/"
	@echo "  make bench-check  One iteration per benchmark (the CI bench lane)"
	@echo "  make bench-compare  Compare two .bench files with benchstat (OLD= NEW=)"
	@echo "  make test-shuffle   Run the tests in random order, three times"
	@echo "  make fuzz        Run the fuzz targets for FUZZTIME (default 15s)"
	@echo "  make stress      Stress one test (PKG= ./pkg RUN=TestName COUNT=200)"
	@echo "  make check-consumers  Build the examples outside the workspace (GOWORK=off)."
	@echo "  make ci          vet + test"
	@echo "  make release-check  Test release tooling, validate changesets, and"
	@echo "                      verify the pending module release plan."
	@echo "  make release-plan   Print the pending module release plan as JSON."
	@echo "  make release-preflight  Verify planned modules stay tidy when released together."
	@echo "  make release-preflight-write  Apply preflight tidy results to go.mod/go.sum."
	@echo "  make release-changelog  Aggregate pending changesets into CHANGELOG.md."

.PHONY: vet
# Opt-in suites (integration_bwrap) are type-checked here even on hosts that
# cannot run them: a file behind a build tag that CI never compiles is a file
# that rots silently. The suites themselves self-skip when their toolchain is
# absent, so running them under test costs nothing where bwrap is not installed.
VET_TAGS = -tags integration_bwrap

vet:
	@$(call GO_FOREACH,$(MODULES_TEST),vet,go vet $(VET_TAGS) ./...)
	@$(call GO_FOREACH,$(MODULES_CONSUMER),vet-consumer,GOWORK=off go vet $(VET_TAGS) ./...)

.PHONY: test
test:
	@$(call GO_FOREACH,$(MODULES_TEST),test,go test $(VET_TAGS) ./... -count=1)
	@$(call GO_FOREACH,$(MODULES_CONSUMER),test-consumer,GOWORK=off go test $(VET_TAGS) ./... -count=1)

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
# (nothing is ever left behind next to the source). For a library module
# it writes no binary but still compiles every package — the pin check a
# reader's own build makes — where `go build -o dir/` refuses to run at
# all ("no main packages to build").
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

# The skill's validator is a consumer as well: its own module, pinned to a
# released `core`, which is the only thing that runs the shipped templates. A
# template that stops loading fails here instead of in somebody's editor, and a
# pin that falls behind the newest tag is a checker reading an older schema.
SKILL_VALIDATOR := skills/flowcraft-deploy/scripts/validator
SKILL_ASSETS := skills/flowcraft-deploy/assets

.PHONY: check-skill check-skill-pin
check-skill: check-skill-pin
	@set -e; out=$$(mktemp -d); trap 'rm -rf "$$out"' EXIT; \
	( cd $(SKILL_VALIDATOR) && GOWORK=off go mod tidy -diff && \
		GOWORK=off go build -o "$$out/validate-config" . ); \
	echo "==> templates"; \
	( cd $(SKILL_ASSETS)/minimal-deploy && \
		WORKSPACE_ROOT="$$(pwd)/workspace" "$$out/validate-config" deploy.yaml ); \
	( cd $(SKILL_ASSETS)/minimal-graph && \
		"$$out/validate-config" --type graph graph.yaml )

# The skill documents one `core` version; a pin behind the newest tag means its
# validator is checking a schema nobody is writing any more.
check-skill-pin:
	@want=$$(git describe --tags --abbrev=0 --match 'core/v*' 2>/dev/null | sed 's|^core/||' || true); \
	got=$$( cd $(SKILL_VALIDATOR) && GOWORK=off go list -m -f '{{.Version}}' github.com/GizClaw/flowcraft/core ); \
	if [ -n "$$want" ] && [ "$$want" != "$$got" ]; then \
		echo "error: $(SKILL_VALIDATOR) pins $$got, the newest core tag is $$want" >&2; \
		echo "bump it: cd $(SKILL_VALIDATOR) && GOWORK=off go get github.com/GizClaw/flowcraft/core@$$want" >&2; \
		echo "         GOWORK=off go mod tidy" >&2; \
		exit 1; \
	fi; \
	echo "==> skill pin $$got"

# Correctness and stability lanes. None of these gate `make ci`: they answer
# different questions (statistical benchmark matches, ordering-dependent bugs,
# parser inputs, nil paths), and a developer chooses when to pay for them.

# Every module in the workspace that carries benchmarks; a module omitted from
# this list is a module whose benchmarks never run. The provider adapters live in
# `inference` now, and their request compilation is on the model-call path, so
# the module that holds them is here; `sandbox` carries the process floor every
# tool pays before it does any work of its own.
BENCH_MODULES ?= core sandbox inference agent tool
BENCH_LABEL ?= run

.PHONY: bench
bench:
	@mkdir -p .bench
	@set -e; for m in $(BENCH_MODULES); do \
		label="$${m//\//-}-$(BENCH_LABEL)"; \
		echo "==> bench $$m -> .bench/$$label.txt"; \
		( cd $$m && go test -run '^$$' -bench . -benchmem -count=5 ./... ) \
			> ".bench/$$label.txt"; \
	done

# bench-check is what CI runs: one iteration per benchmark, so every benchmark
# compiles, runs and reports. Numbers are not compared — a shared runner varies
# enough that a threshold would be noise. The allocation guards live in the
# tests and gate through `make test`.
.PHONY: bench-check
bench-check:
	@$(call GO_FOREACH,$(BENCH_MODULES),bench-check,go test -run '^$$' -bench . -benchmem -benchtime=1x ./...)

.PHONY: bench-compare
bench-compare:
	@if [ -z "$(OLD)" ] || [ -z "$(NEW)" ]; then \
		echo "usage: make bench-compare OLD=.bench/core-old.txt NEW=.bench/core-new.txt" >&2; \
		exit 2; \
	fi
	@go run golang.org/x/perf/cmd/benchstat@latest "$(OLD)" "$(NEW)"

# Coverage for the sandbox subsystem, measured across the packages rather than
# per package. A per-package number is misleading here: sessionkit and netpolicy
# are exercised through the backends, so their own test files cover a small slice
# while the real hit rate is several times higher.
.PHONY: cover
cover:
	@cd core && go test -count=1 -coverpkg=./sandbox/... -coverprofile=../.cover.out ./sandbox/... >/dev/null
	@cd core && go tool cover -func=../.cover.out | tail -n 25
	@echo "==> full profile: .cover.out (go tool cover -html=.cover.out)"

.PHONY: test-shuffle
test-shuffle:
	@$(call GO_FOREACH,$(MODULES_TEST),test-shuffle,go test -shuffle=on -count=3 ./...)
	@$(call GO_FOREACH,$(MODULES_CONSUMER),test-shuffle-consumer,GOWORK=off go test -shuffle=on -count=3 ./...)

.PHONY: fuzz
fuzz:
	@scripts/fuzz.sh

.PHONY: stress
stress:
	@if [ -z "$(PKG)" ] || [ -z "$(RUN)" ]; then \
		echo "usage: make stress PKG=./agent RUN=TestAgentRun COUNT=200" >&2; \
		exit 2; \
	fi
	@scripts/stress.sh "$(PKG)" "$(RUN)" "$(COUNT)"

.PHONY: ci
ci: vet test

.PHONY: release-check
release-check:
	@cd scripts/releasegate && GOWORK=off go test -count=1 ./...
	@if [[ -n "$(BASE)" ]]; then \
		cd scripts/releasegate && GOWORK=off go run . validate --repo ../.. --base "$(BASE)"; \
	else \
		cd scripts/releasegate && GOWORK=off go run . validate --repo ../..; \
	fi
	@cd scripts/releasegate && GOWORK=off go run . plan --repo ../..

.PHONY: release-plan
release-plan:
	@cd scripts/releasegate && GOWORK=off go run . plan --repo ../.. --json

.PHONY: release-preflight
release-preflight:
	@cd scripts/releasegate && GOWORK=off go run . preflight --repo ../..

.PHONY: release-preflight-write
release-preflight-write:
	@cd scripts/releasegate && GOWORK=off go run . preflight --repo ../.. --write

.PHONY: release-changelog
release-changelog:
	@cd scripts/releasegate && GOWORK=off go run . changelog --repo ../.. --write
