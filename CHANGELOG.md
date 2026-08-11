# Changelog

All notable changes to this repository are documented here. FlowCraft is a
multi-module monorepo; each Go module is released independently with its own tag
prefix, for example `sdk/vX.Y.Z`, `memory/vX.Y.Z`, and `sdkx/vX.Y.Z`.

Pending changesets are aggregated into module release sections by the automated
Release PR before their tags are published.

## Current Published State

| Module   | Latest tag      | Notes                                                                                     |
| -------- | --------------- | ----------------------------------------------------------------------------------------- |
| `sdk`    | `sdk/v0.5.3`    | Core agent, engine, graph, LLM, tool, workspace, event, and telemetry primitives.         |
| `memory` | `memory/v0.1.7` | Standalone memory-domain module: recall, history, knowledge, retrieval, text, and stores. |
| `sdkx`   | `sdkx/v0.5.5`  | Provider/adaptor release pinned to `sdk v0.4.8` and `memory v0.1.7`.                      |

## [Unreleased]

_No pending changes._

<!-- releasegate:releases -->

## `sdkx/v0.5.5` - 2026-08-11

### Changed

- sdkx patch: hosted web_search via ProviderOutputs, dynamic tool injection / lazy MCP attach / policy middleware / session wiring, session lifecycle observer, sandbox sessions and net policy, bwrap CI alignment

## `sdk/v0.5.3` - 2026-08-11

### Changed

- sdk patch: sandbox ProcessManager sessions, rule-based net policy, MITM, process events; pty Resize race fix; dynamic tool injection and policy middleware; hosted web_search capability metadata

## `sdkx/v0.5.4` - 2026-08-10

### Changed

- Move script runtimes to sdkx/agent/script/{jsrt,luart} and keep deprecated alias packages marked for 0.6.0 removal

## `sdk/v0.5.2` - 2026-08-10

### Changed

- Harden agent checkpoint contract: shared Validate/Clone, strict exec-id matching, graph spec-version drift detection, and a CheckpointStore conformance suite

## `sdkx/v0.5.3` - 2026-08-10

### Changed

- Add configurable SQLite and workspace agent checkpoint stores (deploy resources + runtime checkpoint_store wiring), session-level end-to-end resume, and align A2A resume admission with the hardened agent checkpoint contract (shared Validate, strict exec-id matching)

## `sdkx/v0.5.2` - 2026-08-10

### Changed

- Lower message DataPart to text in generate and embed compilers across sdkx providers

## `sdk/v0.5.1` - 2026-08-09

### Changed

- Expose an owned route policy clone on the inference config Assembly

## `sdkx/v0.5.1` - 2026-08-09

### Changed

- Use the deployment-less Azure Responses path and gate reasoning includes on model capability

## `sdkx/v0.5.0` - 2026-08-08

### Changed

- sdkx v0.5.0: unify provider adapters under sdkx/inference, land deploy/runtime/scheduler/delegation and A2A assembly, swap nsjail for bubblewrap, and remove the legacy 0.4.x surfaces

## `sdk/v0.5.0` - 2026-08-08

### Changed

- Breaking cleanup: remove v0.4.x deprecated surfaces and land the unified runtime (engine into agent, llm/embedding/model into inference+message), the config.Factory/Source/Loader build protocol, and bubblewrap sandbox with network enforcement

### Removed

- Retired the `vessel` runtime and `vesseld` daemon from the active source
  tree, CI, release automation, examples, and documentation. Existing tags and
  release artifacts are retained as historical, unsupported releases.
- Removed the deprecated `sdk/history`, `sdk/recall`, `sdk/knowledge`,
  `sdk/retrieval`, and `sdk/textsearch` packages in favor of the standalone
  `memory` module.
- Removed the legacy knowledge graph node, recall v1 SQLite queue, and history
  and knowledge tool adapters tied to those SDK contracts, along with the
  retrieval compatibility adapters, namespace migration utility, and legacy
  retrieval E2E module.
- Removed `sdkx/tool/memory`, the built-in file-tree Memory Tool adapter over
  `sdk/workspace`. Model-driven file memory is an application concern: build it
  on `sdk/workspace` in application-owned code, or attach a filesystem MCP
  server through `sdkx/tool/mcp`.

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
