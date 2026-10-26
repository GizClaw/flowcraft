# shellcheck shell=bash
# shellcheck disable=SC1088,SC1036,SC2283,SC2035,SC2046,SC2034,SC2091
# ShellCheck is a shell linter and has no Make parser: variable assignments
# (`?=`), make functions (`$(wildcard ...)`, `$(1)`), and recipe lines are read
# as shell, so those codes fire on every Makefile and mean nothing here. The
# comment must not start with "shellcheck": the tool would parse the prose as a
# malformed directive (SC1072/SC1073).
# Pin bash so the GO_FOREACH macro's `set -e; for ... done` semantics are
# stable across hosts (default sh on some macOS setups treats `set -e`
# inside compound statements differently).
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Modules listed in go.work — `go vet ./...` and friends work as-is.
BACKEND_MODULES := $(patsubst %/go.mod,%,$(wildcard backends/*/go.mod backends/*/*/go.mod))
# The provider adapters share one module: `driver` provides every
# `driver/<name>` package, so a new provider is a package, not a module.
# Spelled out rather than globbed — a wildcard that stops matching a
# consolidated module silently drops it from every target below.
DRIVER_MODULES := driver

MODULES_WORK := core craft $(BACKEND_MODULES) $(DRIVER_MODULES) examples/forge examples/anvil

# Wire contracts live in proto/ (shared by the Rust platform and the Go SDK).
# Go stubs are generated into their Go packages; Rust generates at build time.
BUF ?= buf
PROTO_GO_DIRS := core/workspace/v1 core/sandbox/v1 core/secret/v1 \
	core/agent/checkpoint/v1 core/tool/v1 core/craft/v1

# Modules gated by CI's gofmt -s + golangci-lint lanes.
MODULES_LINT := core craft $(BACKEND_MODULES) $(DRIVER_MODULES)

# The Rust platform is its own workspace with a pinned toolchain
# (rust/rust-toolchain.toml) and its own build cache, so it is not a go.work
# module and the GO_FOREACH lanes never touch it. The `rust-*` targets below are
# the equivalent lanes and `ci`/`lint` include them: a change that breaks the
# Rust side has to fail the same pre-commit gate the Go side does.
RUST_DIR := rust
CARGO ?= cargo

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
	@echo "  make lint        Run golangci-lint, cargo clippy and cargo fmt --check"
	@echo "  make tidy        Run go mod tidy on all modules"
	@echo "  make rust-test   Run cargo test on the Rust workspace"
	@echo "  make rust-fmt    Run cargo fmt on the Rust workspace"
	@echo "  make rust-clippy Run cargo clippy -D warnings on the Rust workspace"
	@echo "  make rust-windows-check Check the workspace capability for a Windows target"
	@echo "  make rust-proto-gen  Regenerate the committed Rust wire bindings (needs protoc)"
	@echo "  make rust-proto-check Regenerate the Rust bindings and fail on drift"
	@echo "  make check-consumers  Build the examples outside the workspace (GOWORK=off),"
	@echo "                      plus craft itself; exemptions must still be exempt."
	@echo "  make proto-lint  Lint the wire contracts in proto/ (needs buf)"
	@echo "  make proto-gen   Regenerate the Go stubs from proto/ (needs buf)"
	@echo "  make proto-check Regenerate and fail on drift in the generated Go stubs"
	@echo "  make proto-breaking  Reject breaking changes against main (needs buf)"
	@echo "  make ci          vet + test, including the Rust workspace"
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
	@$(MAKE) --no-print-directory rust-fmt-check rust-clippy

.PHONY: tidy
tidy:
	@$(call GO_FOREACH,$(MODULES_WORK),tidy,go mod tidy)

# The Rust lanes. `rust-fmt` is the fixer and `rust-fmt-check` is what `lint`
# calls, mirroring the `fmt`/CI-gofmt split above rather than inventing a second
# convention. `rust-clippy` is `-D warnings` because the tree is clean today:
# a new warning should cost a review comment, not a follow-up commit.
.PHONY: rust-test
rust-test:
	@echo "==> test $(RUST_DIR)"
	@cd $(RUST_DIR) && $(CARGO) test --workspace

.PHONY: rust-fmt
rust-fmt:
	@echo "==> fmt $(RUST_DIR)"
	@cd $(RUST_DIR) && $(CARGO) fmt --all

.PHONY: rust-fmt-check
rust-fmt-check:
	@echo "==> fmt check $(RUST_DIR)"
	@cd $(RUST_DIR) && $(CARGO) fmt --all -- --check

.PHONY: rust-clippy
rust-clippy:
	@echo "==> clippy $(RUST_DIR)"
	@cd $(RUST_DIR) && $(CARGO) clippy --workspace --all-targets -- -D warnings

# The kernel's transport is unix-domain sockets, so the platform as a whole is
# unix-only. The workspace capability is not: with `--no-default-features` it
# keeps the provider, its decorators and the helper operations, and drops the
# `wire` feature (endpoint, remote client, grant scope) that needs the control
# plane. This target is the local feedback loop — a cross-compile, so the
# provider's `cfg(windows)` branches are compiled rather than merely written. It
# needs `rustup target add x86_64-pc-windows-gnu` and a mingw gcc, which is why
# it is not part of `lint`/`ci`; the `rust-windows` CI lane runs the same shape
# on a Windows host, where those branches also execute.
RUST_WINDOWS_TARGET := x86_64-pc-windows-gnu

.PHONY: rust-windows-check
rust-windows-check:
	@echo "==> check $(RUST_DIR) for $(RUST_WINDOWS_TARGET)"
	@cd $(RUST_DIR) && $(CARGO) clippy -p flowcraft --all-targets \
		--no-default-features --features workspace \
		--target $(RUST_WINDOWS_TARGET) -- -D warnings

# Modules are installed with GOBIN pointed at a temp directory: `go
# install` writes whatever binary a module produces outside the tree
# (nothing is ever left behind next to the source). For a library module
# like `craft` it writes no binary but still compiles every package — the
# pin check a reader's own build makes — where `go build -o dir/` refuses
# to run at all ("no main packages to build").
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
ci: vet test rust-test

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

# ── Wire contracts ──
# proto/ is the single source of truth for every cross-process contract. The Go
# SDK generates its stubs from these files, and so does the Rust crate — but the
# Rust bindings are committed, so anything that builds `flowcraft` (a consumer,
# docs.rs, `cargo install`) needs neither `protoc` nor a build script.
# `rust-proto-gen` refreshes them; `rust-proto-check` is the drift gate, the
# counterpart of `proto-check` below.

.PHONY: proto-gen
proto-gen:
	@BUF="$(BUF)" python3 proto/generate.py

.PHONY: proto-check
proto-check: proto-gen
	@git diff --exit-code -- $(PROTO_GO_DIRS) || { \
		echo "generated Go stubs drifted: run 'make proto-gen' and commit the result" >&2; \
		exit 1; \
	}
	@test -z "$$(git status --porcelain -- $(PROTO_GO_DIRS) | grep '^??')" || { \
		echo "untracked generated files: commit them" >&2; \
		git status --porcelain -- $(PROTO_GO_DIRS); \
		exit 1; \
	}

.PHONY: proto-lint
proto-lint:
	@command -v $(BUF) >/dev/null 2>&1 || { echo "$(BUF) is required: https://buf.build/docs/installation"; exit 1; }
	cd proto && $(BUF) lint

# The Rust side of the same gate. `flowcraft/src/proto/generated/` is checked in,
# and the generator is an example target, so no build script and no
# build-dependency reach a consumer.
.PHONY: rust-proto-gen
rust-proto-gen:
	@command -v protoc >/dev/null 2>&1 || { echo "protoc is required to regenerate the bindings"; exit 1; }
	@echo "==> regenerate $(RUST_DIR) wire bindings"
	@cd $(RUST_DIR) && $(CARGO) run -q -p flowcraft --example gen_proto

.PHONY: rust-proto-check
rust-proto-check: rust-proto-gen
	@git diff --exit-code -- $(RUST_DIR)/flowcraft/src/proto || { \
		echo "generated Rust bindings drifted: run 'make rust-proto-gen' and commit the result" >&2; \
		exit 1; \
	}
	@test -z "$$(git status --porcelain -- $(RUST_DIR)/flowcraft/src/proto | grep '^??')" || { \
		echo "untracked generated files: commit them" >&2; \
		git status --porcelain -- $(RUST_DIR)/flowcraft/src/proto; \
		exit 1; \
	}

.PHONY: proto-breaking
proto-breaking:
	@command -v $(BUF) >/dev/null 2>&1 || { echo "$(BUF) is required: https://buf.build/docs/installation"; exit 1; }
	cd proto && $(BUF) breaking --against "../.git#branch=main"
