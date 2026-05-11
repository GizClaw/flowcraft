# eval/longmemeval

[LongMemEval](https://arxiv.org/abs/2410.10813) (ICLR 2025) baseline for
flowcraft's long-term-memory pipeline.

LongMemEval is the modern successor to LoCoMo: 500 questions over chat
histories up to **500 sessions / 115k tokens**, testing five abilities
that current memory systems still struggle on:

| Type | Tests |
| --- | --- |
| `single-session-user` | Extracting a fact stated by the user in one session |
| `single-session-assistant` | Recalling what the assistant said in one session |
| `single-session-preference` | Picking up an implicit user preference |
| `multi-session` | Reasoning across 2+ sessions |
| `temporal-reasoning` | When-did / order-of-events questions |
| `knowledge-update` | New value supersedes an older one stated earlier |
| `*_abs` (abstention) | The answer is *not* in the haystack — system must refuse |

Why add this on top of `eval/locomo`: by 2026 most production memory
systems clear ~80% on LoCoMo, so the benchmark no longer separates good
systems from great ones. LongMemEval is currently the headline
benchmark cited in every new memory paper (Letta, Mem0, MemEval,
LoCoMo-Plus, …); having flowcraft baseline numbers on it is what makes
our memory claims comparable to the field.

## Re-use of eval/locomo

This package intentionally ships **only a converter**, no new runner.
LongMemEval's data shape maps cleanly onto `eval/dataset.Dataset`, so
once converted it runs through `eval/locomo/cmd/eval` unmodified. That
keeps the prompts, providers, judge, reranker, and CLI flags identical
between the two suites — when we say "+5pp from enabling reranker" the
number is comparable across LoCoMo and LongMemEval.

The split exists so that LongMemEval-specific tuning (per-type prompts,
abstention-aware scoring) has a home as the suite matures.

## Quick start

```bash
# 1) Download the three official files (cleaned 2025-09 version)
mkdir -p eval/longmemeval/data && cd eval/longmemeval/data
wget https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json
wget https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json
wget https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_m_cleaned.json
cd ../../..

# 2) Convert each into our .jsonl shape
GOWORK=off go run ./cmd/eval longmemeval convert \
    -in  longmemeval/data/longmemeval_oracle.json \
    -out longmemeval/data/longmemeval_oracle.jsonl

GOWORK=off go run ./cmd/eval longmemeval convert \
    -in  longmemeval/data/longmemeval_s_cleaned.json \
    -out longmemeval/data/longmemeval_s.jsonl

# 3) Run via the locomo CLI (same flags, same prompts)
export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
GOWORK=off go run ./cmd/eval locomo run \
    --dataset      longmemeval/data/longmemeval_s.jsonl \
    --extractor                                          \
    --extractor-llm   deepseek                           \
    --answer-llm      minimax                            \
    --judge-llm       azure                              \
    --judge-style     locomo                             \
    --reranker-llm    deepseek                           \
    --embedder        qwen:text-embedding-v4             \
    --topk            30                                 \
    --concurrency     8                                  \
    --ingest-concurrency 8                               \
    --progress-every  50                                 \
    --out             longmemeval/results/lme-s-baseline.json
```

The output JSON is the same `locomo.Report` shape, so existing
`eval/locomo/cmd/compare` works without changes.

## Per-type breakdown

The converter tags each question with `qtype:<type>` plus `abs` for
abstention instances. To get per-type scores out of a finished report:

```bash
jq -r '
  .per_question
  | group_by(.tags // [])
  | map({type: (.[0].tags // [""]) | join(","), n: length, judge: (map(.judge) | add / length)})
' longmemeval/results/lme-s-baseline.json
```

(The locomo `Report.PerQuestion` field doesn't currently carry the
question's tags; if/when that gets added, this jq one-liner shortens to
a plain group-by.)

## Three flavors, three runtimes

| File | Sessions/instance | Approx tokens | Recommended use |
| --- | --- | --- | --- |
| `longmemeval_oracle.json` | only evidence sessions | <5k | smoke / sanity check |
| `longmemeval_s.json` | ~40 | ~115k | **headline baseline** |
| `longmemeval_m.json` | ~500 | ~1.5M | scalability / stress |

On flowcraft's current stack (deepseek-flash extractor +
minimax answer + azure judge + deepseek reranker, k=30) `_s` takes
roughly 4-5h end-to-end and `_m` is closer to 30-40h — gate the latter
behind a manual nightly until ingest concurrency is tuned upward.

## Baselines

Headline `oracle` numbers from periodic full-sweep runs. Raw JSON
reports are NOT committed (gitignored under `results/`); ask the
on-call eval owner for the artefact if you need the per-question
detail. Numbers are recorded here so the trend is visible at a
glance from the repo.

| Date       | Stack (extractor / answer / judge / reranker / embedder)                                                              | k  | n   | qa.judge | qa.em | qa.f1 | recall.p95 | save.p95 |
| ---------- | --------------------------------------------------------------------------------------------------------------------- | -- | --- | -------- | ----- | ----- | ---------- | -------- |
| 2026-05-11 | deepseek-v4-flash / minimax-m2.7-highspeed / azure-gpt-5 (locomo prompt) / qwen-flash / qwen text-embedding-v4         | 30 | 500 | **0.812**| 0.484 | 0.134 | 7.5 s      | 244 s    |

The 2026-05-11 sweep was the first oracle baseline against
`feat/eval-migration`'s unified runner — see PR #91 for the code
under test. mem0's published LongMemEval oracle reference range is
~0.70-0.80 on qa.judge, so flowcraft's 0.812 is a competitive
starting point; gains from here are expected to come from per-type
prompts (the `*_abs` abstention class still drags) rather than the
core pipeline.

## Citation

```bibtex
@article{wu2024longmemeval,
  title  = {LongMemEval: Benchmarking Chat Assistants on Long-Term Interactive Memory},
  author = {Di Wu and Hongwei Wang and Wenhao Yu and Yuwei Zhang and Kai-Wei Chang and Dong Yu},
  year   = {2024},
  url    = {https://arxiv.org/abs/2410.10813}
}
```
