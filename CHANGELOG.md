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
  disk check, log tee, and idle watchdog.
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
  Headline metric is *attempted accuracy* (rewards calibration over
  hallucination); per-topic breakdown on by default.
- `eval/taubench`: Go-native [τ-bench](https://arxiv.org/abs/2406.12045)
  tool-use suite — single-shot and multi-turn (LLM-as-customer) modes,
  retail + airline domains with bundled mini-datasets, upstream task
  JSON loader with shadow-run scoring. NOT a PR gate (LLM-call-heavy;
  weekly/release-time use only).

### Changed
- **Breaking (internal)**: evaluation code consolidated under `eval/`,
  freeing the word `bench` for Go's `Benchmark*` performance
  benchmarks. Shared `dataset/` and `metrics/` packages were promoted
  to `eval/`'s top level; all suites now share a single off-workspace
  module `github.com/GizClaw/flowcraft/eval`. External consumers
  update imports per the table below:

  | Old | New |
  |---|---|
  | `…/bench/locomo` | `…/eval/locomo` |
  | `…/bench/locomo/dataset` | `…/eval/dataset` |
  | `…/bench/locomo/metrics` | `…/eval/metrics` |
  | `…/bench/locomo/runners…` | `…/eval/locomo/runners…` |
  | `…/bench/history-compression` | `…/eval/history` |
  | `…/tests/quality/knowledge` | `…/eval/knowledge` |

---

## `vesseld/v0.1.0-rc.1` — 2026-05-07

First release candidate of the `vesseld` orchestration daemon.

### Added
- Declarative YAML configuration for vessels, agents, LLM clients, and rate
  limiters.
- HTTP control plane: `/v1/vessels/{id}/call`, `/submit`, `/logs` (SSE),
  `/v1/runs` (paginated), `/metrics` (Prometheus text exposition).
- Multi-vessel fleet supervision with per-vessel concurrency gates.
- Cross-platform release artifacts via `release-vesseld.yml`.

## `vessel/v0.1.0-rc.2` — 2026-05-07

### Added
- `Handle.OnTerminate` synchronous lifecycle hook for ordered termination
  side-effects (run registry, concurrency gate release, etc.).

## `vessel/v0.1.0-rc.1` — 2026-05-07

First release candidate of the `vessel` runtime: in-process Captain managing
agents, shared memory, engines, supervision, and probes (token-budget,
tool-reachable, custom).

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

## `sdkx/v0.2.5` and earlier

Provider implementations: OpenAI, Anthropic, Google, OpenAI-compatible
runtimes; SQLite and Postgres+pgvector backends for `sdk/retrieval`.

## `voice/v0.2.0`

Voice pipeline: STT → LLM → TTS with VAD and WebRTC transport.
