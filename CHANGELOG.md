# Changelog

All notable changes to this repository are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
loosely follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html) on a
per-module basis.

FlowCraft is a multi-module monorepo. Each module is released independently
with its own tag prefix (e.g. `sdk/vX.Y.Z`, `vessel/vX.Y.Z`,
`vesseld/vX.Y.Z`). Per-release artifacts and notes also live on the
[GitHub Releases](https://github.com/GizClaw/flowcraft/releases) page.

## [Unreleased]

### Added

#### sdk

- `sdk/engine`: capabilities / resume / revise lifecycle. New
  `engine/depname` package centralises Dependencies keys (LLMClient,
  ToolRegistry, ToolAllowedNames, …) so engine authors and host
  wiring stop relying on stringly-typed map keys. New
  `engine.WithCapabilities` helper + `engineWithCaps` adapter lets
  any engine declare a `Describer` without re-implementing the round
  driver, finally honouring the long-standing "engines may declare
  capabilities / may be resumable / Decider may ask for a revise"
  contract in the public godocs.
- `sdk/engine` + `sdk/tool`: built-in `ask_user` tool with
  host-on-context plumbing. New `engine.WithHost(ctx, host)` /
  `engine.HostFromContext(ctx)` helpers let any tool reach
  `engine.Host.AskUser` from inside `tool.Tool.Execute`. The
  llmnode round driver now wraps the tool dispatch context with
  the live `Host`. Contract is advisory: tools that truly require a
  host must surface `errdefs.NotAvailable` when absent.
- `sdk/agent`: `WithParentRunID(string) RunOption` threads a parent
  run ID through `runConfig` and stamps it onto every
  `engine.Run.ParentRunID` the revise loop dispatches. Closes the
  contract-audit gap where `engine.Run.ParentRunID` had been a typed
  field with zero writers since v0.1; multi-agent call-chain loop /
  depth detection now has the data it always claimed to have.
- `sdk/agent`: `WithArtifactChannels(channels ...string) RunOption`
  harvests board channels into `Result.Artifacts` at the end of a
  run. Closes the contract-audit gap where `Result.Artifacts` had
  been documented since v0.1 but no `agent.Run` code path
  populated it — hosts that wrote artifact channels previously saw
  the data vanish at the agent boundary.
- `sdk/agent`: `Agent.Tools` finally takes effect as a policy gate.
  `agent.Run` now promotes the agent's tool list into
  `engine.Run.Deps[depname.ToolAllowedNames]` once per run, and
  `llmnode.Node.resolveTools` enforces it on each tool dispatch.
  Allow-list semantics: empty list = no gate (back-compat); non-empty
  list = tools outside the set return `errdefs.PermissionDenied`.
- `sdk/agent` + `sdk/graph/node/scriptnode`: `agent.RunInfoFromAttributes`
  reads the standard agent_id / run_id / task_id / context_id
  attribute keys back into a fully-populated `agent.RunInfo`.
  scriptnode now uses it, so scripts see the full run identity
  instead of `RunInfo{RunID: ec.RunID}` with empty AgentID / TaskID /
  ContextID.
- `sdk/graph`: `ExecutionContext.Deps` (`*engine.Dependencies`) and
  `ExecutionContext.Attributes` (`map[string]string`) propagate from
  `engine.Run` down to every node. Tools and scriptnode can now
  resolve host-supplied dependencies (LLM clients, tool registries,
  retrievers, …) without closure-binding them at builder time.
- `sdk/graph/node/llmnode`: tool registry + allow-list resolution
  from `ExecutionContext.Deps`. Per-run `*tool.Registry` and
  per-agent allow-list flow through the graph cleanly; constructor-
  bound registry is still honoured when no run-scoped registry is
  present.
- `sdk/recall`: `Entry.Subject` and `Entry.Predicate` opt-in fields.
  When both are non-empty and slot-eligible (neither contains `|`),
  `Memory.Add` now writes `MetaSubject` / `MetaPredicate` /
  `MetaSlotKey` and participates in the slot supersede channel,
  matching `Memory.Save`'s built-in extractor path. Closes
  [#100](https://github.com/GizClaw/flowcraft/issues/100): callers
  that fan out facts to different scopes per-fact (and therefore
  must drive `Add` instead of `Save`) can now combine per-fact scope
  routing with slot dedup. Backwards-compatible: existing `Add`
  callers that leave both fields zero are unchanged.

#### eval / docs

- Top-level `README.md`, `CHANGELOG.md`, and `SECURITY.md`.
- `eval/cmd/eval`: unified Cobra CLI replaces the per-suite
  `eval/<suite>/cmd/eval` mains. Invoke as `eval <suite>`; LoCoMo +
  LongMemEval expose sub-subcommands (`eval locomo run/convert/
compare/fetch/ingest`, `eval longmemeval convert`). Shell completion
  and uniform `--help` come for free.
- `eval/internal/env`: JSON-encoded `FLOWCRAFT_<ALIAS>` provider
  credentials with alias-to-factory decoupling and capability-aware
  `llm.WithCaps` wrapping. Replaces ad-hoc per-CLI env handling.
- `eval/internal/notify` + `eval/scripts/run-eval.sh`: Feishu CardKit
  notifications (one live-updated card per run; webhook backend
  intentionally unsupported) plus a process supervisor with PID lock,
  disk check, log tee, and idle watchdog. Lifecycle events
  (`ingest_done` / `done` / `error`) post a threaded text reply so
  operators get a phone-buzz at phase boundaries; intra-phase
  `*_progress` milestones stay as silent card edits.
- `eval/locomo`: `--judge-style {locomo|strict}` and
  `--judge-temperature` flags; mem0-aligned LoCoMo judge prompt at
  `metrics.LocoMoLLMJudgePrompt`. Per-batch ingest heartbeats so slow
  extractors don't look frozen.
- `eval/longmemeval`: [LongMemEval](https://arxiv.org/abs/2410.10813)
  (ICLR 2025) converter. Ships no runner of its own — the LoCoMo
  runner drives it, keeping LoCoMo / LongMemEval numbers directly
  comparable across all three official flavours (`oracle` / `_s` /
  `_m`).
- `eval/knowledge`: standardised on the `Run(ctx, ds, opts) → Report`
  shape with a CLI binary and Feishu events. Embedder selection
  collapses onto a single `KNOWLEDGE_EVAL_EMBEDDER` env var via the
  shared `eval/internal/env` loader.
- `eval/beir`: [BEIR](https://arxiv.org/abs/2104.08663)-format
  retrieval suite (graded nDCG@k, binary Recall@k, MRR). Loads the
  canonical 3-file layout directly; reuses `sdk/knowledge`'s embedder
  pipeline.
- `eval/simpleqa`: [SimpleQA](https://openai.com/index/introducing-simpleqa/)
  short-form factuality suite with OpenAI's LLM-as-judge rubric.
  Headline metric is _attempted accuracy_ (rewards calibration over
  hallucination); per-topic breakdown on by default.
- `eval/taubench`: Go-native [τ-bench](https://arxiv.org/abs/2406.12045)
  tool-use suite — single-shot and multi-turn (LLM-as-customer) modes,
  retail + airline domains with bundled mini-datasets, upstream task
  JSON loader with shadow-run scoring. NOT a PR gate (LLM-call-heavy;
  weekly/release-time use only).

### Fixed

#### sdkx

- `sdkx/llm/{openai,anthropic,bytedance}`: status-code-aware error
  classification. The generic `errdefs.ClassifyProvider` regex
  (`\b(?:http|status)\s*(\d{3})\b`) misses the SDK error format used
  by openai-go, anthropic-sdk-go, and volcengine arkruntime — all
  three format their `Error.Error()` as `<METHOD> "<URL>": <code>
  <status>` (or `"Error code: <code>"` for arkruntime), and the
  `"https://"` URL prefix or the literal `"code:"` keyword defeat
  the heuristic. Result: real 400 / 404 / 422 client errors fell
  through to the `ProviderTransient` default → `NotAvailable`,
  which the locomo runner's new retry-once would then quietly
  retry instead of fail-fast. Per-provider `classifyAPIError` now
  routes by `StatusCode` (401/403→Unauthorized, 402→Forbidden,
  429→RateLimit, 400/405/422→Validation, 408/409/≥500→NotAvailable).
  The openai variant additionally splits 404 by `error.code` body
  field so Azure AI Foundry's bare-body "capacity blip" 404s stay
  `NotAvailable` (transient, retryable) while structured
  `DeploymentNotFound` 404s become `Validation` (fail-fast, no
  retry storm on a wrong deployment name). `ollama` and the image
  generators already drive `ClassifyHTTPStatus` / explicit
  `switch resp.StatusCode` so they're unaffected.

- `sdkx/llm/{openai,anthropic,bytedance,ollama,*/image}`: guard every
  chat-completion / image-generation entry point against `(nil, nil)`
  return tuples from upstream SDKs. The openai-go family does in
  fact return `(nil, nil)` when an OpenAI-compatible backend answers
  with literal JSON `null` — a real-world DeepSeek failure mode that
  crashed the LongMemEval `_s` eval runner at ~9% ingest with a
  `nil pointer dereference` at `sdkx/llm/openai/openai.go:314`. The
  anthropic-sdk-go family shares the same pointer-return shape and
  the same latent bug. The pointer-return convention `err==nil ⇒
  resp!=nil` is not a Go language guarantee — guard symmetrically
  across every provider so a flaky upstream is surfaced as a clean
  `errdefs.NotAvailable` instead of taking down the calling
  goroutine. Streaming variants get the matching `nil stream handle`
  check. Regression tests use `httptest` with `body=null` for the
  openai/anthropic families (which reproduces the live failure
  exactly) and a misbehaving `RoundTripper` for ollama.

### Changed

#### eval

- `eval/go.mod`: bump `sdk` v0.3.0 → v0.3.4 and `sdkx` v0.3.0 → v0.3.1.
  Picks up the `sdk/recall.Add` slot-metadata fix (issue #100) and the
  `sdkx/llm` nil-response guards (PR #103) so the LongMemEval `_s`
  runner no longer panics at `sdkx/llm/openai/openai.go:314` when
  DeepSeek's OpenAI-compatible backend answers a queued request with
  literal JSON `null` — failures now surface as clean
  `errdefs.NotAvailable` errors that the per-question scorer reports
  as `error` instead of crashing the whole run.

- **Breaking (internal)**: evaluation code consolidated under `eval/`,
  freeing the word `bench` for Go's `Benchmark*` performance
  benchmarks. Shared `dataset/` and `metrics/` packages were promoted
  to `eval/`'s top level; all suites now share a single off-workspace
  module `github.com/GizClaw/flowcraft/eval`. External consumers
  update imports per the table below:

  | Old                           | New                      |
  | ----------------------------- | ------------------------ |
  | `…/bench/locomo`              | `…/eval/locomo`          |
  | `…/bench/locomo/dataset`      | `…/eval/dataset`         |
  | `…/bench/locomo/metrics`      | `…/eval/metrics`         |
  | `…/bench/locomo/runners…`     | `…/eval/locomo/runners…` |
  | `…/bench/history-compression` | `…/eval/history`         |
  | `…/tests/quality/knowledge`   | `…/eval/knowledge`       |

### Deprecated

#### sdk

- `agent.Request.Extensions`. Agent never interpreted the field, no
  engine ever read it, and the `map[string]any` →
  `map[string]string` type mismatch with `engine.Run.Attributes`
  meant any future forwarding would have needed ad-hoc per-host
  serialisation. Use `agent.WithAttributes(...)` instead — same
  wire format, same codepath, no serialisation guesswork. The
  field is retained for source compatibility but ignored on every
  write path.

---

## `sdk/v0.3.3` — 2026-05-11

### Added

- `sdk/script`: `signal.error` / `signal.interrupt` are now natively
  typed — `Signal` carries an errdefs `Kind` (errors) or `engine.Cause`
  (interrupts) plus a freeform `Detail`. `script.SignalToError` is the
  single mapping point; jsrt / luart accept both the bare-string form
  (back-compat) and `{kind, message, detail}`. `scriptnode` delegates
  to it so `errdefs.Is*` and `engine.Interrupted{Cause}` work through
  the chain. `iteration.js` forwards Kind/Detail when re-raising
  child signals.

## `sdk/v0.3.2` — 2026-05-11

### Fixed

- `sdk/graph/scriptnode`: repaired built-in JS scripts against v0.3
  bindings — `answer.js` now routes through the canonical per-node
  publisher (the v0.2 `stream` global was retired); `approval.js`
  fills `node.id` from a new `scriptnode/bridge_node.go` instead of a
  never-injected `config.__node_id`; `iteration.js` clears
  `__iteration_result` between iterations, checks child signals
  before pushing results, and propagates `signal.{error,interrupt,done}`
  to the parent loop. `builtins_test.go` covers each script through
  the real ScriptNode wiring so future binding renames trip the suite
  instead of breaking silently at runtime.

## `sdk/v0.3.1` — 2026-05-11

### Fixed

- `sdk/graph` + `sdk/graph/node/llmnode`: `graph.ValidateOutputs` now
  accepts a required `PortTypeMessages` output satisfied either via a
  board var (historical) or a non-empty channel of the same name
  (the v0.3 reality). `llmnode.OutputPorts` drops `Required: true`
  for the messages port — redundant with the executor's
  run-then-validate sequence. Without this fix every llmnode round
  driven through `graph/runner.Runner` came back with
  `Status=failed` / `"missing required output port"`, silently
  breaking observers, persistence, and memory-extract pipelines that
  gate on `StatusCompleted`.

## `sdk/v0.3.0` — 2026-05-09

The v0.2.x deprecation window closes in a single coordinated cut.
Every symbol that lived in a per-package `deprecated.go` shim during
v0.2.x is removed. See [`docs/migrations/v0.3.0.md`](docs/migrations/v0.3.0.md)
for the full removed-symbol table and upgrade recipe.

### Removed (breaking)

- `sdk/workflow` and `sdk/graph/adapter` (workflow ↔ graph bridge).
- `sdk/llm`: `RoundResult` / `RunRound` / `StreamRound` /
  `RoundConfig` family, `CapsMiddleware` / `WithExtraCaps` /
  `LookupModelCaps` shims, `ModelInfo.Caps` auto-promotion.
- `sdk/graph`: internal `Executor` interface, `ErrInterrupt`,
  `WithEventBus` / `WithStreamCallback` / `WithCheckpointStore`
  options, `busOnlyHost` shim, `VarMessages` / `VarQuery` /
  `VarAnswer`. Runner collapses onto `engine.Engine`.
- `sdk/knowledge`: legacy v0.1 surface (`Document` / `Chunk` /
  `SearchResult` / `SearchOptions` / `DocInput`, `Store` family,
  `ChunkDocument` / `ScoreChunk` / `RankResults` / `RRFMerge`,
  `ChangeNotifier` / `Reloader`, `KnowledgeConfig` / `Node`).
  Everything routes through `*Service` now.
- `sdk/history`: `Closer` / `Compactor.Close`, `SummaryCacheStore`,
  `RecoverArchive` / `SaveManifest` top-level helpers; tool
  implementations + `ToolDeps` / `RegisterTools` relocate to
  `sdkx/tool/history`.
- `sdk/retrieval`: `SearchRequest.ReturnRaw` and
  `SearchResponse.RawByRetriever`.
- `sdk/script/bindings`: `NewStreamBridge`, `NewRunBridge`,
  `AgentStepBindings`, `BuildEnv` preset. `NewRunInfoBridge` is the
  surviving run-metadata bridge.

### Changed

- `sdk/engine`: `MainChannel` renamed from `""` to `"__main_channel"`
  with a `RestoreBoard` migration shim for old checkpoints. The `__`
  prefix is now reserved for engine-owned board keys.
- `sdk/graph/node/llmnode`: `MessagesKey` →
  `MessagesChannel` (typed channel name); `QueryFallback` removed.
- `sdk/graph/node/knowledgenode`: gains `Config.QueryKey`.
- `sdk/knowledge`: `ContextLayer` ↔ `Layer` flip
  (`ContextLayer` kept as alias); `ModeSemantic` removed.

### Added

- `sdk/model.LastByRole` helper for role-scoped reverse scans.

## `sdkx/v0.3.0` — 2026-05-09

### Changed

- Bump `sdk` dependency to `v0.3.0`. Tool implementations and
  context keys for `sdkx/tool/history` and `sdkx/tool/kanban` take
  ownership of the symbols relocated out of `sdk/` — signatures stay
  identical for downstream callers that swapped imports during the
  v0.2.x deprecation window. See
  [`docs/migrations/v0.3.0.md`](docs/migrations/v0.3.0.md) for the
  full mapping.

### Removed

- `sdkx/knowledge/watcher` (alongside the deprecated
  `knowledge.ChangeNotifier`). Durable watchers must emit
  `ChangeEvent` for `EventReloader` directly.

---

## `vesseld/v0.1.0` — 2026-05-11

General availability of the `vesseld` orchestration daemon. Composes
the `v0.1.0` vessel runtime with declarative YAML, multi-vessel fleet
supervision, and a Prometheus-instrumented HTTP control plane into a
single static binary suitable for production single-node deployments.

The cut goes directly from `vesseld/v0.1.0-rc.1` to GA — no `rc.2`
tag was published. The `vessel` dependency bump to `v0.1.0-rc.2`
landed in `main` shortly after the vessel tag (consumed
`Handle.OnTerminate` so the fleet supervisor can release the
concurrency gate + prune the run registry deterministically on
termination, eliminating a class of latent races surfaced by chaos
tests), and shipped as part of this GA.

### Highlights

- Vessel lifecycle (`/v1/vessels/{id}/call`, `/submit`, `/drain`,
  `/phase`) wired through unix socket **and** TCP + bearer-token auth.
  The TCP listener refuses to start without a `tokenFile`; mTLS is on
  the `v0.2` track.
- Per-vessel concurrency gate, shared LLM clients with cross-vessel
  rate limiting, and the seven catalog-registered probes
  (`TokenBudgetProbe`, `ToolReachableProbe`, `PromptResponseProbe`,
  …) all on by default.
- Fleet supervisor uses `vessel.Handle.OnTerminate` for deterministic
  concurrency-gate release + run-registry pruning on termination
  (chaos-test hardened).
- End-to-end `tests/e2e/vesseld` (25+ test files: allowlist, auth,
  chaos, concurrency, drain, restart, sidecar reject, …) gates the
  release.
- Cross-platform binaries (linux/amd64, linux/arm64, darwin/amd64,
  darwin/arm64, windows/amd64) with SHA-256 checksums via
  `.github/workflows/release-vesseld.yml`.

### Examples

- [`examples/vesseld-multi-vessel/`](examples/vesseld-multi-vessel/) —
  the canonical multi-agent demo with Kanban-style delegation.
- [`examples/vesseld-with-history/`](examples/vesseld-with-history/) —
  single-vessel demo wiring a shared `HistoryStore` so the agent
  remembers earlier turns of the same `context_id` conversation.
  (Cross-restart persistence ships in `v0.2`.)

### Known limitations (tracked for `v0.2`)

- No per-run filesystem session storage (`vessel.WithSessionStore` is
  not yet wired).
- mTLS / SecretProvider / `vesseld migrate` subcommand are not yet
  shipped — runbook recommends fronting the TCP listener with a
  TLS-terminating proxy in the meantime.

## `vesseld/v0.1.0-rc.1` — 2026-05-07

First release candidate of the `vesseld` orchestration daemon.

### Added

- Declarative YAML configuration for vessels, agents, LLM clients, and rate
  limiters.
- HTTP control plane: `/v1/vessels/{id}/call`, `/submit`, `/logs` (SSE),
  `/v1/runs` (paginated), `/metrics` (Prometheus text exposition).
- Multi-vessel fleet supervision with per-vessel concurrency gates.
- Cross-platform release artifacts via `release-vesseld.yml`.

## `vessel/v0.1.0` — 2026-05-11

General availability of the in-process `vessel` runtime. No API-level
changes between `vessel/v0.1.0-rc.2` and GA — this is a stability /
documentation coordination release that pairs with `vesseld/v0.1.0`.

### Highlights

- Captain lifecycle (`Submit` / `Drain` / `Stop` / `Restart`) with
  per-vessel concurrency gates and deterministic termination via
  `Handle.OnTerminate`.
- `Spec.Resources.MaxTokensPerTurn` / `MaxTokensPerHour` budget caps
  enforced end-to-end through `vessel/budget.go` + `vessel/sandbox.go`.
- Built-in probes (`TokenBudgetProbe`, `ToolReachableProbe`,
  `PromptResponseProbe`) for liveness / readiness signals; custom
  probes register via `Probe` interface.
- Multi-agent routing, Kanban agent-as-tool delegation, sidecar
  agents, and shared history across the agent roster.
- Decoupled from `sdk/workflow` (removed in `sdk/v0.3.0`); composes
  cleanly on `sdk/engine` + `sdk/agent`.

### Known limitations (tracked for `v0.2`)

- `WithSessionStore` (per-run isolated filesystem workspace) is not
  yet wired; long-running agents that need disk state should use a
  Captain-scoped workspace today and migrate when `v0.2` lands.

## `vessel/v0.1.0-rc.2` — 2026-05-07

### Added

- `Handle.OnTerminate` synchronous lifecycle hook for ordered termination
  side-effects (run registry, concurrency gate release, etc.).

## `vessel/v0.1.0-rc.1` — 2026-05-07

First release candidate of the `vessel` runtime: in-process Captain managing
agents, shared memory, engines, supervision, and probes (token-budget,
tool-reachable, custom).

## `sdk/v0.2.8` … `sdk/v0.2.11` — 2026-05-07 … 2026-05-08

Incremental v0.2 releases between the v0.2.7 anchor and the v0.3.0
cutover. See individual tags for full detail; headline changes:

- `sdk/v0.2.8` — `sdk/llm` image-output caps (PR #71).
- `sdk/v0.2.9` — handoff primitives + the first round of v0.3
  deprecation shims (PR #77).
- `sdk/v0.2.10` — `sdk/retrieval` scoring refinements (PR #80).
- `sdk/v0.2.11` — `sdk/retrieval` workspace capabilities (PR #81).

## `sdk/v0.2.7` and earlier

See git history under `sdk/v*` tags. Highlights:

- `sdk/agent` — agent orchestration with observers and deciders.
- `sdk/graph` — declarative DAG executor.
- `sdk/engine` — execution primitives (Board, Run, Host, Interrupt,
  Checkpoint, Subject).
- `sdk/recall` — long-term memory with hybrid BM25 + vector + entity
  retrieval, RRF fusion, and entity-boost / time-decay reranking.
- `sdk/history`, `sdk/knowledge`, `sdk/llm`, `sdk/event`, `sdk/model`,
  `sdk/tool`, `sdk/kanban`.

## `sdkx/v0.2.6` … `sdkx/v0.2.9` — 2026-05-07 … 2026-05-08

Incremental v0.2 releases tracking `sdk` v0.2.8–v0.2.11. Headline
changes:

- `sdkx/v0.2.6` — image-modality adapters (PR #72).
- `sdkx/v0.2.7` — Phase-1 checkpoint + OTel (PR #78).
- `sdkx/v0.2.8` — tool migrations from `sdk/{history,kanban}` →
  `sdkx/tool/{history,kanban}` (PR #79).
- `sdkx/v0.2.9` — `retrieval` workspace (PR #82).

## `sdkx/v0.2.5` and earlier

Provider implementations: OpenAI, Anthropic, Google, OpenAI-compatible
runtimes; SQLite and Postgres+pgvector backends for `sdk/retrieval`.

## `voice/v0.2.0`

Voice pipeline: STT → LLM → TTS with VAD and WebRTC transport.
