# eval/

FlowCraft's lightweight evaluation module.

This module intentionally keeps the maintained eval surfaces:

- `cmd/eval/` — Cobra CLI skeleton and shared global flags.
- `simpleqa/` — OpenAI SimpleQA factuality + calibration benchmark.
- `locomo/` — LoCoMo memory eval for QA, event summaries, and
  caption-proxy multimodal dialog generation.
- `internal/env/` — resolves `--*-llm <alias>[:<model>]` into SDK LLM providers.
- `internal/notify/` — optional Feishu CardKit progress notifications.

Older retrieval, history, and tool-use benchmark suites have been removed from
this repo.

## Quick Start

```bash
cd eval
go test ./... -count=1
```

Run SimpleQA:

```bash
curl -L https://openaipublic.blob.core.windows.net/simple-evals/simple_qa_test_set.csv \
  -o /tmp/simple_qa_test_set.csv

export FLOWCRAFT_QWEN='{"provider":"qwen","api_key":"sk-...","model":"qwen-max"}'
export FLOWCRAFT_AZURE='{"provider":"azure","api_key":"...","model":"gpt-5","base_url":"https://..."}'

go run ./cmd/eval simpleqa \
  --dataset /tmp/simple_qa_test_set.csv \
  --answer-llm qwen:qwen-max \
  --judge-llm azure \
  --limit 50 \
  --out /tmp/simpleqa.json
```

Run LoCoMo memory eval:

```bash
go run ./cmd/eval locomo \
  --dataset /var/lib/flowcraft-eval/datasets/locomo10.json \
  --workspace /var/lib/flowcraft-eval/workspaces/locomo-$(date +%s) \
  --answer-llm qwen:qwen-max \
  --tasks qa,event,dialog \
  --limit-samples 1 \
  --limit-qa 5 \
  --out /tmp/locomo.json
```

`locomo` uses `localworkspace_raw_source` memory. It stores source messages under
the same `--workspace` root, indexes those source messages for retrieval, and
forms QA/event/dialog context from retrieval-backed source hits plus recent raw
turns instead of semantic observation/fact/entity stages. The dialog task is reported as
`caption_proxy`: `img_url`, `blip_caption`, and `query` metadata are used as text
inputs, not as official visual MM-R scoring.

## Provider Credentials

CLI LLM flags take the form `--answer-llm <alias>[:<model>]` and
`--judge-llm <alias>[:<model>]`. The alias maps to a JSON environment variable:

```json
{
  "provider": "azure",
  "api_key": "sk-...",
  "model": "gpt-5",
  "base_url": "https://...",
  "api_version": "2024-08-01-preview",
  "caps": { "no_temperature": true }
}
```

Lookup order:

1. `FLOWCRAFT_<ALIAS>`
2. `FLOWCRAFT_TEST_<ALIAS>`

The optional `:<model>` suffix overrides the JSON's `model` field for a
single run.

## Notifications

All commands inherit these flags:

- `--notify-name` — label shown in the Feishu card.
- `--notify-progress-pct` — progress milestone resolution; `0` disables intermediate updates.
- `--notify-dry-run` — print notification events to stderr.

Real Feishu delivery reads credentials from `FEISHU_APP_ID`,
`FEISHU_APP_SECRET`, and `FEISHU_CHAT_ID`.

## CI

The top-level `make eval` target runs:

```bash
cd eval && go test ./... -count=1
```

The GitHub Actions PR gate runs the same package test sweep as part of
`.github/workflows/ci.yml`. Manual eval workflows live in
`.github/workflows/eval-simpleqa.yml` and `.github/workflows/eval-locomo.yml`.
