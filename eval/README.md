# eval/

FlowCraft's lightweight evaluation module.

This module intentionally keeps only the current baseline eval surface:

- `cmd/eval/` — Cobra CLI skeleton and shared global flags.
- `simpleqa/` — OpenAI SimpleQA factuality + calibration benchmark.
- `internal/env/` — resolves `--*-llm <alias>[:<model>]` into SDK LLM providers.
- `internal/notify/` — optional Feishu CardKit progress notifications.

Older memory, retrieval, history, and tool-use benchmark suites have been
removed from this repo.

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

## Provider Credentials

CLI flags take the form `--answer-llm <alias>[:<model>]` and
`--judge-llm <alias>[:<model>]`. The alias maps to a JSON environment
variable:

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
`.github/workflows/ci.yml`. The only manual eval workflow kept in
`.github/workflows/` is `eval-simpleqa.yml`.
