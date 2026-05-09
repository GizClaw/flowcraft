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

### Changed
- **Breaking (internal)**: 评测代码统一迁移到 `eval/` 目录，命名对齐 AI 行业
  `eval` 约定（区分于 Go 的 `Benchmark*` 性能基准）。`bench/locomo` →
  `eval/locomo`，`bench/history-compression` → `eval/history`，
  `tests/quality/knowledge` → `eval/knowledge`。共享的 `dataset/` 与
  `metrics/` 包提升到 `eval/` 顶层；新增 `eval/report/` 作为 v0.4 统一
  Report schema 的落地点（当前仅占位）。`bench/` 目录与独立的
  `tests/quality/knowledge/go.mod` 已删除，三个 suite 现在共享单个
  off-workspace 模块 `github.com/GizClaw/flowcraft/eval`。Makefile 新增
  `make eval` / `make eval-smoke`，CI 新增 `test-eval` lane。外部 import
  受影响的项目按下表替换 import path 即可：

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
