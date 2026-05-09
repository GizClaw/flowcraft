# eval/

FlowCraft 的质量评测套件。

> 注意：这里是 **AI/ML 意义上的 eval**（accuracy / F1 / judge），
> 不是 Go 的 `Benchmark*` 性能基准。性能基准用 `*_test.go` 里的
> `Benchmark*` 函数即可，本目录不收录。

## 套件清单

| Suite | 测什么 | 入口 |
|---|---|---|
| `locomo/` | 长期记忆（recall）的 EM / F1 / qa.judge / recall.k_hit | `go run ./locomo/cmd/eval` |
| `history/` | history compactor 的质量与 token 成本 trade-off | `go run ./history/cmd/eval` |
| `knowledge/` | knowledge retrieval（BM25/vector/hybrid）质量回归 | `go test -tags=integration ./knowledge/...` |

## 共享包

- `dataset/` —— LoCoMo 风格 conversation/question schema
- `metrics/` —— EM、F1、LLM-as-Judge、Latency 聚合
- `report/` —— 统一 Report schema 与 compare（演进中，v0.4 落地）

## 模块边界

`eval/` 是 **off-workspace** 的独立 Go module（`go.mod` 与主仓库 `go.work` 解耦），
通过 `require` 直接 pin 已发布的 sdk / sdkx 版本。这样：

- 100MB 级别的 LoCoMo 语料、judge prompt、报告产物都不会污染 sdk 的 patch 发布；
- 评测跑的就是「外部用户实际拉到的字节」，质量数据有可比性；
- bumping sdk pin 是手工 PR，不会被 sdk 的 auto-tag 流程拽着走。

跑 eval 套件请始终带上 `GOWORK=off`，或者用顶层 `make eval` / `make eval-smoke`
封好的目标。

## 快速开始

```bash
# 0) 全套：vet + 单元
make eval

# 1) LoCoMo synthetic（无网络、无 LLM，~1s）
GOWORK=off go run ./locomo/cmd/eval --dataset synthetic --out /tmp/locomo.json

# 2) LoCoMo10（10 个对话、1.5k 问题，~1m，无 LLM 仅 EM/F1）
git clone https://github.com/snap-research/locomo eval/locomo/data/locomo
GOWORK=off go run ./locomo/cmd/convert-locomo \
    -in  eval/locomo/data/locomo/data/locomo10.json \
    -out eval/locomo/data/locomo10.jsonl
GOWORK=off go run ./locomo/cmd/eval \
    --dataset eval/locomo/data/locomo10.jsonl \
    --out     eval/locomo/results/locomo10.json

# 3) history compactor（需要 QWEN_API_KEY 之类，否则只跑 none/buffer）
export QWEN_API_KEY=sk-...
GOWORK=off go run ./history/cmd/eval \
    --dataset      eval/locomo/data/locomo10.jsonl \
    --answer-llm   qwen:qwen-max \
    --summary-llm  qwen:qwen-turbo \
    --judge-llm    qwen:qwen-max \
    --out          /tmp/history.json

# 4) knowledge retrieval（默认 BM25 通道无凭据；integration tag 走 vector/hybrid）
GOWORK=off go test ./knowledge/... -count=1
EMBEDDING_PROVIDER=qwen EMBEDDING_API_KEY=sk-... EMBEDDING_MODEL=text-embedding-v3 \
    GOWORK=off go test -tags=integration ./knowledge/... -count=1
```

`eval/locomo/data/`、`eval/locomo/results/`、`eval/history/results/` 都被
`eval/.gitignore` 排除：上游语料是 CC-BY 但体量大；报告是 per-run 产物。

## CI 接入

- **PR gate**：`make eval`（即 `cd eval && GOWORK=off go test ./... -count=1`），
  跑 synthetic 数据集，无需 API key。`.github/workflows/ci.yml` 里有专门的
  `test-eval` job，已挂进 `ci-pass` gate。
- **Nightly**：跑 LoCoMo10 + 完整 history compactor eval（依赖 secret，待补）。

## 历史

`eval/locomo` 此前在 `bench/locomo`；`eval/history` 此前在
`bench/history-compression`；`eval/knowledge` 此前在 `tests/quality/knowledge`。
统一搬到 `eval/` 后命名对齐 AI 行业惯例（区分于 Go 的性能 benchmark），
也把 `dataset/` / `metrics/` 提到顶层方便 LoCoMo / history 共享。
