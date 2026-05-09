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
- `eval/internal/env`：评测 CLI 共享的 provider 凭据加载器。读 JSON-encoded env
  var (`FLOWCRAFT_<ALIAS>` 优先，`FLOWCRAFT_TEST_<ALIAS>` 兜底)，shape 与
  `sdk/llm.NewFromConfig` 的 `config map[string]any` 对齐。Alias 和 provider
  分离：spec `azure_reasoning:gpt-5.4` 中 `azure_reasoning` 命名 env 变量、
  factory 名从 JSON 的 `"provider"` 字段读，所以一个 provider 可挂多份连接
  profile（不同 api_key / caps / base_url）。`BuildLLM` 自动叠加 catalog
  ModelCaps + 用户 `caps` 覆盖并用 `llm.WithCaps` 包裹返回值，避免 DeepSeek
  等 provider 因 `WithJSONSchema` 透传到不支持 `json_schema` 的模型而 400。
- `eval/locomo`：`--judge-style {locomo|strict}`（默认 `locomo`，对齐 mem0
  官方 evaluation 的 judge prompt）和 `--judge-temperature`（默认 0，确定性）
  两个 flag。`metrics.LocoMoLLMJudgePrompt` 复刻 mem0 仓库的 LLM-judge prompt
  ——明确"be generous"、放宽日期格式、要求一句话理由——便于跨项目的 qa.judge
  数字横向对齐。Ingest 阶段新增 per-batch 心跳日志（每个 Save 完成一行），
  让慢 extractor / 限速场景下不再"看起来卡住"。

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
