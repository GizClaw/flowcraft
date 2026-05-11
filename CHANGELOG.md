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
- `eval/cmd/eval`: unified Cobra-powered CLI that replaces the seven
  per-suite `eval/<suite>/cmd/eval` main packages. Every suite now
  registers itself via a public `RegisterCobra(parent, *cliflags.Global)`
  constructor (`eval/<suite>/cli.go`), and the root binary owns the
  shared persistent flags (`--env-file`, `--out`, `--verbose`,
  `--notify-*`). LoCoMo gains discoverable sub-subcommands for its
  auxiliary tools — `eval locomo run / convert / compare / fetch /
  ingest` — and LongMemEval's converter moves to `eval longmemeval
  convert`. Auto-generated `--help`, shell-completion (`eval
  completion bash|zsh|fish`) and consistent `eval <suite> --help`
  navigation come along for free. The Makefile's `eval-smoke` target
  and every README example switch to the new `go run ./cmd/eval …`
  invocation; old paths like `go run ./locomo/cmd/eval` no longer
  exist. `eval/internal/cliflags` hosts the shared global flag set
  and a `WriteReport` helper that replaces ~10 lines of
  `json.MarshalIndent` + `--out vs stdout` boilerplate previously
  duplicated across every suite's `main()`.
- `eval/internal/env`: shared provider-credential loader for the evaluation
  CLIs. Reads a single JSON-encoded env var (`FLOWCRAFT_<ALIAS>` preferred,
  `FLOWCRAFT_TEST_<ALIAS>` fallback) whose shape mirrors
  `sdk/llm.NewFromConfig`'s `config map[string]any`. Alias is decoupled
  from factory: in `azure_reasoning:gpt-5.4` the token `azure_reasoning`
  names the env var while the factory name is read from the JSON
  `"provider"` field, so one provider can register multiple connection
  profiles (different api_key / caps / base_url). `BuildLLM` merges the
  catalog `ModelCaps` with any user `caps` overrides and wraps the bare
  client with `llm.WithCaps`. Without this, options like `WithJSONSchema`
  used to leak through to providers that reject `json_schema` (e.g.
  DeepSeek returning 400) because eval CLIs bypass `DefaultResolver`.
- `eval/locomo`: `--judge-style {locomo|strict}` (default `locomo`,
  mem0-aligned) and `--judge-temperature` (default `0`, deterministic)
  flags. `metrics.LocoMoLLMJudgePrompt` ports mem0's official LoCoMo
  judge prompt — explicit "be generous" instruction, date-format
  leniency, one-sentence reasoning step — so `qa.judge` is comparable
  with mem0's published numbers. The original strict prompt remains
  available under `--judge-style strict` for backward compat. The
  ingest stage also emits a per-batch heartbeat line (one log line per
  `Save`) so slow extractors and rate-limit walls are visible
  immediately instead of looking frozen.
- `eval/internal/notify` + `eval/scripts/run-eval.sh`: Feishu CardKit
  notifications for long-running evals. Both runners (`locomo` and
  `history`) emit structured lifecycle events via their respective
  `Options.{Hook,ProgressPct}` fields and a shared CLI flag set
  (`--notify-name`, `--notify-progress-pct`, `--notify-dry-run`)
  registered via the new `notify.CLIFlags` helper. Locomo / LongMemEval
  emit `start`, `ingest_progress`, `ingest_done`, `qa_progress`,
  `done`, plus a one-shot `error` on high QA failure rate. History
  compression emits `start`, `strategy_start`, `strategy_progress`,
  `strategy_done`, `done`, plus per-strategy `error`. Each run becomes
  ONE live-updated CardKit card whose markdown body is rewritten in
  place on every event, so a 50-hour LongMemEval run produces a single
  chat message instead of hundreds. Credentials are read from env
  (`FEISHU_APP_ID`, `FEISHU_APP_SECRET`, `FEISHU_CHAT_ID`) so they
  never appear in `ps` output. A companion `eval/scripts/run-eval.sh`
  wraps the binary as a pure process supervisor: PID-file lock,
  pre-flight disk check, log tee, and a 30-min log-idle watchdog. The
  Feishu custom-bot webhook backend is deliberately not supported — at
  evaluation timescales it floods the destination chat.
- `eval/taubench`: upstream τ-bench JSON loader — `LoadUpstreamTasks()`
  reads the official task JSON (`tau_bench/envs/<domain>/tasks.json`)
  alongside the paired initial DB state, **shadow-executes** each
  task's gold action list against a clone of the initial state, and
  pins the resulting snapshot as `ExpectedFinalState`. Scoring then
  requires the agent's post-run State to deep-equal that snapshot
  AND every fragment from the upstream `outputs` array to appear in
  the agent's final reply (case-insensitive substring match,
  configurable via `ExpectedTextFragments`). Unknown tool names in
  the gold trace abort the loader with the offending action index —
  no silent skipping. CLI grows `--upstream-tasks` and
  `--upstream-initial-state` flags. The schema is documented inline
  at `upstream.go::UpstreamTask`; upstream column-name shifts call
  for a loader bump rather than mutating the on-disk JSON.
- `eval/taubench`: airline domain — adds `NewAirlineTools()` (7 tools:
  `get_user`, `get_reservation`, `list_user_reservations`,
  `cancel_reservation`, `update_baggage`, `search_flight`,
  `get_flight`) and `NewAirlineMiniDataset()` (5 hand-curated tasks
  covering cancel / update-baggage / search-info / refuse-departed,
  plus one multi-turn cancel-via-lookup dialog). The CLI grows a
  `--domain airline` value and `--domain all` merges both packs into
  a single report. `taubench.MergeDatasets()` ships as a public
  helper for the merge; tool handlers tolerate stringified ints
  (a common provider quirk).
- `eval/taubench`: multi-turn dialog mode lands — second LLM
  (`Options.CustomerLLM`) roleplays the customer using a private
  `Task.CustomerScenario` while the agent only sees the customer's
  natural-language utterances (never tool calls or tool results).
  The customer terminates the dialog with a configurable stop token
  (default `###STOP###`); both `MaxAgentTurns` and a new
  `MaxConversationTurns` cap stalled runs. `TaskResult.Mode` ("single-shot"
  | "multi-turn") + `CustomerTurns` field surface the split in the
  report. The single-shot path from the previous commit is preserved
  unchanged. README emphasises this suite is **NOT a PR gate** —
  multi-turn runs cost ~3 000 LLM calls per 100 tasks, suitable for
  weekly/release-time regression only.
- `eval/taubench`: new tool-use suite — a Go-native re-implementation
  of [τ-bench](https://arxiv.org/abs/2406.12045)'s single-turn
  instruction variant. The customer's full goal is fed to the agent
  in one user message and the agent then chains tool calls until it
  either succeeds (StateChecks pass + RequiredTools were called) or
  hits the `--max-turns` ceiling. A bundled retail-mini task pack
  (5 hand-curated tasks covering cancel / update-shipping / search /
  refuse-protected-state) + 6 retail tools (`get_order`,
  `list_orders_for_customer`, `cancel_order`, `update_shipping`,
  `get_product`, `search_products`) lets a smoke run execute with
  zero external assets. The harness re-implements the loop natively
  rather than wrapping the Python upstream so eval/ stays a single
  Go module with no Python toolchain in CI. Three actionable failure
  modes are surfaced on each `TaskResult.Reason` (state mismatch /
  required tool never called / max-turns exceeded). Roadmap:
  LLM-as-customer multi-turn dialog harness, airline-domain tools,
  and a converter for the upstream task JSON.
- `eval/simpleqa`: new [SimpleQA](https://openai.com/index/introducing-simpleqa/)
  suite (4 326 short-form factual questions, LLM-as-judge graded with
  OpenAI's official rubric). Headline metric is *attempted accuracy*
  (`CORRECT / (CORRECT + INCORRECT)`) which rewards calibration —
  a model that abstains rather than hallucinates scores higher even
  at lower raw accuracy. Accepts both the upstream CSV
  (`simple_qa_test_set.csv`) and a converted JSONL form; the verdict
  parser mirrors OpenAI's `\b(A|B|C)\b` regex so judge replies like
  "Honestly I AM not sure" are NOT mis-bucketed as CORRECT on the
  stray 'A'. Per-topic breakdown is on by default so per-category
  regressions surface alongside the headline number. The current eval
  is closed-book; a follow-up commit will add a knowledge-grounded
  variant that wraps the answer LLM with `sdk/knowledge.Search` so
  raw-model vs RAG-augmented calibration can be measured side by side.
- `eval/beir`: new public-dataset retrieval suite that drives
  `sdk/knowledge` against any [BEIR](https://arxiv.org/abs/2104.08663)
  task (SciFact, NFCorpus, FiQA, …) and emits the metrics the BEIR
  leaderboard publishes — graded nDCG@k, binary Recall@k, MRR. Same
  `Run(ctx, ds, opts) → Report` shape as `eval/knowledge` and shares
  its embedder pipeline, but the scoring layer differs because BEIR
  qrels carry per-query *graded* relevance (0/1/2) rather than a single
  ExpectedDoc. Internal metric helpers (`dcg`, `nDCG`, `recall`, `mrr`)
  ship with closed-form unit tests so a wrong-log-base or off-by-one
  in the rank loop is caught independently of the retrieval pipeline.
  `cmd/eval` accepts BEIR's canonical 3-file layout (`corpus.jsonl`,
  `queries.jsonl`, `qrels/test.tsv`) directly, no converter step
  required.
- `eval/knowledge`: lifted retrieval-quality suite from `go test`-only
  into the standard `Run(ctx, ds, opts) → Report` shape and added a
  `cmd/eval` binary. Lanes (`bm25` / `vector` / `hybrid`) can now be
  scored from CI, a script, or a notebook with identical metrics and a
  JSON report. The integration suite migrates off the legacy
  `EMBEDDING_PROVIDER` + `EMBEDDING_API_KEY` + `EMBEDDING_MODEL` env
  trio onto a single `KNOWLEDGE_EVAL_EMBEDDER="qwen:text-embedding-v4"`
  variable resolved via the shared `eval/internal/env` loader, so one
  `.env` now unlocks every eval suite. Emits the same Feishu CardKit
  events as locomo / history (`start`, `lane_start`, `lane_progress`,
  `lane_done`, `done`) — short runs typically finish before the second
  milestone fires but the framework is identical for future
  10k-document corpora. BM25 lane still runs in CI by default (no
  credentials needed); recall@5 stays pinned at the historical 1.00
  baseline.
- `eval/longmemeval`: [LongMemEval](https://arxiv.org/abs/2410.10813)
  (ICLR 2025) baseline suite. 500 independent instances, each carrying
  its own ~40-session haystack, covering the five core long-term-memory
  abilities (information extraction, multi-session reasoning, temporal
  reasoning, knowledge update, abstention). This suite intentionally
  ships **only a converter** — `cmd/convert` maps the upstream JSON
  onto our `eval/dataset` schema and `locomo/cmd/eval` runs the rest,
  so prompt / judge / reranker / flag behaviour stays identical
  between LoCoMo and LongMemEval and numbers are directly comparable.
  Covers all three official flavors (`oracle` / `_s` / `_m`) which range
  from ~30 min to tens of hours depending on stack. See
  `eval/longmemeval/README.md`.

### Changed
- **Breaking (internal)**: evaluation code consolidated under `eval/`,
  aligning the naming with the AI/ML "eval" convention (and freeing the
  word `bench` for Go's built-in `Benchmark*` performance benchmarks).
  `bench/locomo` → `eval/locomo`, `bench/history-compression` →
  `eval/history`, `tests/quality/knowledge` → `eval/knowledge`. The
  shared `dataset/` and `metrics/` packages were promoted to `eval/`'s
  top level; a new `eval/report/` placeholder anchors the v0.4 unified
  Report schema. The old `bench/` directory and the standalone
  `tests/quality/knowledge/go.mod` were removed — the four suites now
  share a single off-workspace module
  `github.com/GizClaw/flowcraft/eval`. `make eval` and `make eval-smoke`
  targets were added and the CI gained a dedicated `test-eval` lane.
  External consumers update their imports per the table below:

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
