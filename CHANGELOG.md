# Changelog

All notable changes to this repository are documented here. FlowCraft is a
multi-module monorepo; each Go module is released independently with its own tag
prefix, for example `sdk/vX.Y.Z`, `memory/vX.Y.Z`, `sdkx/vX.Y.Z`, and
`voice/vX.Y.Z`.

Per-release artifacts and generated notes also live on the
[GitHub Releases](https://github.com/GizClaw/flowcraft/releases) page.

## Current Published State

| Module | Latest tag | Notes |
| --- | --- | --- |
| `sdk` | `sdk/v0.4.0` | Core agent, engine, graph, LLM, tool, workspace, event, and telemetry primitives. |
| `memory` | `memory/v0.1.0` | First standalone memory-domain release; current HEAD focuses on sources, views, retrieval, text, and execution substrate packages. |
| `sdkx` | `sdkx/v0.4.0` | Provider/adaptor release pinned to `sdk v0.4.0`; current HEAD keeps concrete provider, tool, checkpoint, and protocol bindings. |
| `voice` | `voice/v0.2.0` | Voice pipeline module. |

## [Unreleased]

- Removed the current support surface for the legacy vessel/vesseld runtime and
  the retired memory service subpackages; current work is centered on `sdk`,
  `memory` sources/views/retrieval/text, `sdkx`, `voice`, and `eval/simpleqa`.

## `sdk/v0.4.0` - 2026-05-30

Coordinated release boundary for the recall v2 architecture and memory-module
split.

### Changed

- Kept `sdk` as the foundation module for agent execution, graph runtime, LLM
  contracts, tools, events, telemetry, and workspace primitives.
- Established the dependency floor consumed by `memory/v0.1.0`,
  `sdkx/v0.4.0`, and `vessel/v0.3.0`.
- Preserved deprecated compatibility surfaces that point users toward the new
  `memory` module where appropriate; the compatibility removal remains a later
  minor-release decision.

## `memory/v0.1.0` - 2026-05-30

First standalone release of FlowCraft's memory-domain module.

### Added

- `memory/recall`: recall v2 write/read pipeline with temporal facts,
  projection materialization, multi-lane retrieval, reranking, repair/triage
  hooks, and evaluation diagnostics.
- `memory/history`: transcript buffers, compacted history, summary stores, and
  related persistence contracts.
- `memory/knowledge`: knowledge service, local/retrieval-backed stores, and
  factory helpers.
- `memory/retrieval`: in-memory, SQLite, Postgres, workspace, scoring, journal,
  namespace, and pipeline packages.
- `memory/text`: tokenization, normalization, BM25, phrase, stemming, stopword,
  quote, and timex helpers.

### Changed

- Pins `github.com/GizClaw/flowcraft/sdk` to `v0.4.0`.
- Promotes recall/history/knowledge/retrieval/text APIs from "SDK subdomain" to
  a first-class, independently versioned module.

## `sdkx/v0.4.0` - 2026-05-30

Provider and adapter release coordinated with `sdk/v0.4.0` and
`memory/v0.1.0`.

### Changed

- Pins `github.com/GizClaw/flowcraft/sdk` to `v0.4.0`.
- Adds an explicit `github.com/GizClaw/flowcraft/memory v0.1.0` dependency for
  deprecated retrieval wrappers that forward to `memory/retrieval/...`.
- Refreshes provider dependency sums after the memory split.

### Included From The v0.3.x Line

- Prompt-cache token accounting across OpenAI-compatible, Anthropic-family, and
  ByteDance adapters, including `TokenUsage.CachedInputTokens` population and
  cache-aware telemetry.
- Provider-specific error classification for OpenAI, Anthropic, ByteDance,
  Azure/OpenAI-compatible wrappers, and related image/streaming paths.
- Nil-response guards for upstream SDKs that can return `(nil, nil)`.
- `sdkx/sandbox/nsjail` as the first sandbox backend that enforces network and
  cgroup resource policy on Linux.

## `vessel/v0.3.0` - 2026-05-30

Runtime release coordinated with the published `sdk` and `memory` modules.

### Added

- `vessel/assembly`: helpers for assembling runtime components around memory
  recall, knowledge, tool catalogs, manifests, and workspace-backed backends.

### Changed

- Pins `github.com/GizClaw/flowcraft/sdk` to `v0.4.0`.
- Pins `github.com/GizClaw/flowcraft/memory` to `v0.1.0`.
- Removes local `replace` directives from the published module.
- Refreshes `vessel/go.sum` against the published `sdk` and `memory` tags.

## `vessel/v0.2.0` - 2026-05-14

Runtime hardening release.

### Added

- `SessionStore` plus `WithSessionStore` for per-run workspace isolation.
- `MemorySessionStore` and `FilesystemSessionStore`.
- `Captain.Resume`, fleet resume support, and daemon `/resume` endpoint.

### Changed

- Actor terminology converged on agent/run fields in lifecycle envelopes.
- Expanded contract, honesty, and end-to-end lifecycle coverage for tools,
  hosts, deciders, observers, resume, ask-user, revise, and HTTP flows.

## `vesseld/v0.1.0` - 2026-05-11

General availability of the standalone daemon.

### Added

- Declarative YAML for vessels, agents, LLM profiles, history stores, probes,
  sidecars, resources, and fleet-level runtime configuration.
- HTTP/SSE control plane over Unix sockets and authenticated TCP.
- Prometheus metrics, run registry, drain/phase endpoints, and e2e tests.
- Cross-platform binary release artifacts.

### Later Mainline Additions

- Declarative sandbox resources, shared session store config, mTLS config,
  secret providers, and TLS resolver helpers landed after `vesseld/v0.1.0`.
  These are documented in current README/examples and will be captured by the
  next daemon binary release tag.

## `sdk/v0.3.x` - 2026-05-09 to 2026-05-17

The v0.3 line closed the v0.2 deprecation window and cleaned up the SDK runtime
surface. See [`docs/migrations/v0.3.0.md`](docs/migrations/v0.3.0.md) for the
full removed-symbol table and migration recipe.

### Highlights

- Removed `sdk/workflow`, workflow/graph adapters, round helper families, and
  legacy graph/runtime shims.
- Converged graph execution on `engine.Engine`, typed message channels, and
  explicit board channel names.
- Removed legacy `sdk/knowledge`, `sdk/history`, and `sdk/retrieval`
  compatibility surfaces that had been deprecated through v0.2.
- Added runtime metadata, host dependencies, tool registry/allow-list plumbing,
  cached-token usage fields, and telemetry attributes/counters.
- Fixed scriptnode bindings, required message-output validation, provider error
  classification, nil upstream responses, and eval module dependency drift.

## `sdkx/v0.3.x` - 2026-05-09 to 2026-05-17

Provider-adapter line paired with `sdk/v0.3.x`.

### Highlights

- Moved tool implementations for history, knowledge, and kanban into
  `sdkx/tool/...` while keeping public signatures stable for migrated callers.
- Added prompt-cache routing/markers and cache-token normalization for supported
  providers.
- Added nsjail sandbox backend and provider-name telemetry overrides.
- Removed `sdkx/knowledge/watcher` alongside the SDK knowledge watcher cleanup.

## `vessel/v0.1.0` - 2026-05-11

General availability of the in-process `vessel` runtime.

### Highlights

- Captain lifecycle: `Submit`, `Drain`, `Stop`, `Restart`, and
  `Handle.OnTerminate`.
- Per-vessel concurrency gates, token budgets, probes, sidecars, multi-agent
  routing, Kanban agent-as-tool delegation, and shared history.
- Decoupled from the removed `sdk/workflow` package and composed on
  `sdk/engine` plus `sdk/agent`.

## `sdk/v0.2.x` and `sdkx/v0.2.x`

The v0.2 line introduced the agent/engine/graph runtime, knowledge/history
redesigns, image-output capabilities, handoff primitives, retrieval scoring,
workspace capabilities, and the first wave of `sdkx/tool/...` migration
wrappers.

## Earlier Releases

See git tags for full detail. The early release line established the core SDK,
provider adapters, retrieval and history primitives, the voice pipeline, and the
first vessel/vesseld release candidates.
