# eval/simpleqa

OpenAI's [SimpleQA](https://openai.com/index/introducing-simpleqa/)
benchmark — 4 326 short-form factual questions covering diverse
topics, graded by an LLM-as-judge using the official rubric.

## Why SimpleQA, given we already ship LoCoMo etc.

| Suite | Tests |
|-------|-------|
| `eval/locomo`, `eval/longmemeval` | memory recall under a known context |
| `eval/history` | prompt-compaction quality vs token cost |
| `eval/knowledge`, `eval/beir` | retrieval (BM25 / vector / hybrid) |
| **`eval/simpleqa`** | model's **factual ceiling + calibration** |

The headline metric is **AttemptedAccuracy** = CORRECT / (CORRECT + INCORRECT).
A model that says "I don't know" on a question it's unsure about scores
HIGHER than a model that confidently hallucinates — explicitly the
behaviour we want from a reliable agent backbone.

## Quick start

```bash
# 1. Fetch the upstream CSV (downloaded once, ~3 MB).
curl -L https://openaipublic.blob.core.windows.net/simple-evals/simple_qa_test_set.csv \
    -o /tmp/simple_qa_test_set.csv

# 2. Run; --answer-llm is the model under test, --judge-llm is the grader.
#    Use a strong judge (gpt-5 / o3 / claude-opus) for trustworthy verdicts.
export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
export FLOWCRAFT_AZURE='{"api_key":"...","model":"gpt-5","base_url":"..."}'

cd eval
GOWORK=off go run ./simpleqa/cmd/eval \
    --dataset    /tmp/simple_qa_test_set.csv \
    --answer-llm qwen:qwen-max \
    --judge-llm  azure \
    --concurrency 8 \
    --out        /tmp/simpleqa-qwenmax.json
```

The CLI accepts both the upstream CSV and a JSONL form. A JSONL row
matches the `Question` struct:

```json
{"id":"q0001","problem":"...","answer":"...","topic":"...","answer_type":"..."}
```

so a converter step is optional — most users will simply point `--dataset`
at the CSV.

## Metrics

| Field | Formula | Interpretation |
|-------|---------|----------------|
| `accuracy` | `CORRECT / N` | raw correctness |
| `attempted_accuracy` | `CORRECT / (CORRECT + INCORRECT)` | **headline**; rewards calibration |
| `abstention_rate` | `NOT_ATTEMPTED / N` | how often the model declined |
| `hallucination_rate` | `INCORRECT / N` | how often the model answered confidently wrong |
| `judge_failures` | judge replies we couldn't parse | sanity: should be ~0 |

A `PerTopic` breakdown is reported when `--include-topic-breakdown` is
on (default). Each row carries the same four ratios for its slice of
the dataset.

## Roadmap: Agentic variants

The current eval treats the model as a closed-book oracle: just the
question goes in, an answer comes out. Future variants will plug
`sdk/knowledge` (RAG over a corpus) or `sdk/agent` + search tools in
front of the answer LLM. The same `Run` function is reused; only the
prompt-building lambda changes. Numbers from the closed-book run
provide the "no augmentation" baseline against which the augmented
flavours can be measured.
