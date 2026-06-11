# eval/simpleqa

OpenAI's [SimpleQA](https://openai.com/index/introducing-simpleqa/)
benchmark: 4,326 short-form factual questions graded by an LLM-as-judge
using the official rubric.

SimpleQA is the baseline eval kept in this repository. It measures a
model's factual ceiling and calibration: a model that says "I don't
know" on uncertain questions scores better than one that confidently
hallucinates.

## Quick Start

```bash
# 1. Fetch the upstream CSV once.
curl -L https://openaipublic.blob.core.windows.net/simple-evals/simple_qa_test_set.csv \
    -o /tmp/simple_qa_test_set.csv

# 2. Run; --answer-llm is the model under test, --judge-llm is the grader.
export FLOWCRAFT_QWEN='{"provider":"qwen","api_key":"sk-...","model":"qwen-max"}'
export FLOWCRAFT_AZURE='{"provider":"azure","api_key":"...","model":"gpt-5","base_url":"..."}'

cd eval
go run ./cmd/eval simpleqa \
    --dataset /tmp/simple_qa_test_set.csv \
    --answer-llm qwen:qwen-max \
    --judge-llm azure \
    --concurrency 8 \
    --out /tmp/simpleqa-qwenmax.json
```

The CLI accepts both the upstream CSV and JSONL rows matching the
`Question` struct:

```json
{"id":"q0001","problem":"...","answer":"...","topic":"...","answer_type":"..."}
```

## Metrics

| Field | Formula | Interpretation |
| --- | --- | --- |
| `accuracy` | `CORRECT / N` | Raw correctness |
| `attempted_accuracy` | `CORRECT / (CORRECT + INCORRECT)` | Headline calibration metric |
| `abstention_rate` | `NOT_ATTEMPTED / N` | How often the model declined |
| `hallucination_rate` | `INCORRECT / N` | How often the model answered wrong |
| `judge_failures` | judge replies we could not parse | Should stay near zero |

`--include-topic-breakdown` is on by default and adds per-topic versions
of the same ratios.
