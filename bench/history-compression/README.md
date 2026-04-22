# bench/history-compression

Quality + token-cost evaluation of `sdk/history` compactor strategies.

Where `bench/locomo` answers _"did recall surface the right fact?"_, this
bench answers _"given a long transcript and no recall layer, can the model
still answer correctly with a compressed history?"_. The two questions
stress orthogonal subsystems — locomo benchmarks the long-term-memory
pipeline (`sdk/recall`), this one benchmarks the short-term-memory
compactor (`sdk/history`) in isolation.

## Strategies

| Strategy   | What it does                                              | Why                                  |
| ---------- | --------------------------------------------------------- | ------------------------------------ |
| `none`     | Pass the whole transcript verbatim                        | Quality upper bound; cost upper bound |
| `buffer`   | Keep only the last N messages (`history.NewBuffer`)       | Cheap baseline; truncation regression test |
| `compacted`| `history.NewCompacted` (DAG summarizer + budget)          | The system under test               |

For each strategy we report `qa_judge` / `qa_em` / `qa_f1` plus prompt token
percentiles and history-load latency. Running all three lets you read off
the compactor's compression ratio _and_ its quality cost in one shot.

When the dataset carries `evidence_id`s on its turns (LoCoMo does, the
synthetic fixture does not), the report also includes:

| field              | meaning                                                                         |
| ------------------ | ------------------------------------------------------------------------------- |
| `evidence_measured`| Questions that carried at least one `evidence_id`                              |
| `truncated`        | Of those, how many lost _all_ evidence turns from the loaded prompt            |
| `truncated_rate`   | `truncated / evidence_measured`                                                |

Reading: `truncated_rate` for `none` is always 0 (the upper bound). A non-zero
value for `compacted` is the cleanest signal that the compactor's TokenBudget
is set too aggressively for the dataset at hand — it lets you separate
"the model failed to use the evidence" (judge ↓ but truncated = 0) from
"the compactor compressed the evidence away" (judge ↓ and truncated > 0).

## Quick start

```bash
# unit smoke (no LLM, runs in <1s)
GOWORK=off go test ./bench/history-compression/... -count=1

# full eval (LoCoMo10 dataset, qwen extractor + judge)
export QWEN_API_KEY=sk-...
GOWORK=off go run ./bench/history-compression/cmd/eval \
    --dataset      bench/locomo/data/locomo10.jsonl \
    --answer-llm   qwen:qwen-max \
    --summary-llm  qwen:qwen-turbo \
    --judge-llm    qwen:qwen-max \
    --out          results/history-compression-locomo10.json
```

Skip the `compacted` strategy by leaving `--summary-llm` empty (it will be
auto-skipped with a `skipped: "summary-llm not configured"` field in the
report).

## Layout

```
bench/history-compression/
├── doc.go         package overview
├── eval.go        Run() + per-strategy loop + token/latency stats
├── eval_test.go   stub-LLM smoke test
└── cmd/eval/      CLI entry
```

The dataset/metrics packages are reused from `bench/locomo` — there is no
new schema to learn.
