# eval/

FlowCraft's quality-evaluation suites.

> Naming note: this is **AI/ML "eval"** (accuracy / F1 / LLM-as-judge),
> not Go's `Benchmark*` performance benchmarks. Performance benchmarks
> live alongside their package as `Benchmark*` functions in `*_test.go`
> and are not collected here.

## Suite catalogue

| Suite          | What it tests                                              | Entry point                                                |
| -------------- | ---------------------------------------------------------- | ---------------------------------------------------------- |
| `locomo/`      | Long-term memory (recall) — LoCoMo benchmark               | `go run ./locomo/cmd/eval`                                 |
| `longmemeval/` | Long-term memory (recall) — LongMemEval (ICLR 2025)        | `go run ./longmemeval/cmd/convert` + `locomo/cmd/eval`     |
| `history/`     | History compactor quality vs. token-cost trade-off          | `go run ./history/cmd/eval`                                |
| `knowledge/`   | Knowledge retrieval (BM25 / vector / hybrid) regressions   | `go run ./knowledge/cmd/eval` *or* `go test ./knowledge/...` |
| `beir/`        | BEIR-format retrieval baselines (nDCG@k / Recall@k / MRR)   | `go run ./beir/cmd/eval --root <beir-dataset>`              |
| `simpleqa/`    | SimpleQA short-form factuality + calibration (LLM-as-judge) | `go run ./simpleqa/cmd/eval --dataset simple_qa_test_set.csv` |

`longmemeval` deliberately ships no runner of its own: the data schema is
compatible with LoCoMo, so once converted the same `locomo/cmd/eval`
runs it end-to-end. This keeps prompts, judge model, reranker, and CLI
flags identical between the two suites — a number like "qwen-flash
reranker is 5× faster than deepseek-flash" is then directly comparable
across LoCoMo and LongMemEval reports.

## Shared packages

- `dataset/` — LoCoMo-style conversation/question schema.
- `metrics/` — EM, F1, LLM-as-judge, latency aggregation.
- `report/` — unified Report schema and compare CLI (evolving; lands in v0.4).
- `internal/env/` — resolves `--*-llm <alias>[:<model>]` CLI flags into
  the `(provider, model, config)` triple consumed by
  `sdk/llm.NewFromConfig`. Details below under "Provider credentials".

## Provider credentials

CLI flags take the form `--answer-llm <alias>[:<model>]`. The `<alias>`
names the env var; the optional `:<model>` suffix overrides the model
embedded in the JSON.

Credentials are passed as a **single JSON env var** whose shape mirrors
`sdk/llm.NewFromConfig`'s `config map[string]any`:

```json
{
  "provider": "azure",
  "api_key": "sk-...",
  "model": "gpt-5.4",
  "base_url": "https://...",
  "api_version": "2024-08-01-preview",
  "caps": { "no_temperature": true }
}
```

Lookup order (first non-empty wins):

1. `FLOWCRAFT_<ALIAS>` — preferred.
2. `FLOWCRAFT_TEST_<ALIAS>` — reuses `tests/conformance/llm`'s existing `.env`.

`<ALIAS>` is the token before the `:` in the spec, upper-cased; it
usually equals the provider name. You can also register multiple aliases
that share a provider, to mount different connection profiles:

```bash
# One Azure resource, two cap sets
export FLOWCRAFT_AZURE_REASONING='{"provider":"azure","api_key":"...","model":"o1-mini","caps":{"no_temperature":true}}'
export FLOWCRAFT_AZURE_FAST='{"provider":"azure","api_key":"...","model":"gpt-4o-mini"}'

go run ./locomo/cmd/eval \
    --extractor-llm azure_reasoning  \
    --answer-llm    azure_fast       \
    --judge-llm     azure_reasoning  \
    --embedder      qwen:text-embedding-v4
```

When you do this the alias no longer equals the factory — the factory
name is read from the JSON's `"provider"` field.

## Module boundary

`eval/` is an **off-workspace** Go module: its `go.mod` is intentionally
decoupled from the main repo's `go.work` and `require`s pinned, already-
released `sdk` / `sdkx` versions directly. That gives us three things:

- 100MB-class LoCoMo / LongMemEval corpora, judge prompts, and report
  artifacts cannot pollute `sdk` patch releases.
- Eval suites run against "the bytes external users actually pull",
  keeping quality numbers comparable across releases.
- Bumping the pinned `sdk` is a manual PR — `sdk`'s auto-tag pipeline
  can't drag the eval module along sideways.

When running anything under `eval/` always set `GOWORK=off`, or use the
top-level `make eval` / `make eval-smoke` targets which wrap that for
you.

## Quick start

```bash
# 0) full sweep: vet + unit
make eval

# 1) LoCoMo synthetic (no network, no LLM, ~1s)
GOWORK=off go run ./locomo/cmd/eval --dataset synthetic --out /tmp/locomo.json

# 2) LoCoMo10 (10 conversations, ~1.5k questions, ~1m without an LLM)
git clone https://github.com/snap-research/locomo eval/locomo/data/locomo
GOWORK=off go run ./locomo/cmd/convert-locomo \
    -in  eval/locomo/data/locomo/data/locomo10.json \
    -out eval/locomo/data/locomo10.jsonl
GOWORK=off go run ./locomo/cmd/eval \
    --dataset eval/locomo/data/locomo10.jsonl \
    --out     eval/locomo/results/locomo10.json

# 3) LongMemEval oracle (500 instances; ~2-4h on deepseek-flash extractor)
mkdir -p eval/longmemeval/data
wget -O eval/longmemeval/data/longmemeval_oracle.json \
    https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json
GOWORK=off go run ./longmemeval/cmd/convert \
    -in  eval/longmemeval/data/longmemeval_oracle.json \
    -out eval/longmemeval/data/longmemeval_oracle.jsonl
# Then run with `locomo/cmd/eval --dataset longmemeval/data/longmemeval_oracle.jsonl ...`

# 4) history compactor (needs FLOWCRAFT_QWEN; otherwise only none/buffer run)
export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
GOWORK=off go run ./history/cmd/eval \
    --dataset      eval/locomo/data/locomo10.jsonl \
    --answer-llm   qwen:qwen-max \
    --summary-llm  qwen:qwen-turbo \
    --judge-llm    qwen:qwen-max \
    --out          /tmp/history.json

# 5) knowledge retrieval (BM25 lane needs no credentials; integration tag
#    or the standalone binary unlock the vector/hybrid lanes)
GOWORK=off go test ./knowledge/... -count=1

# integration suite: single env var picks the embedder alias defined in .env
KNOWLEDGE_EVAL_EMBEDDER=qwen:text-embedding-v4 \
    GOWORK=off go test -tags=integration ./knowledge/... -count=1

# or run the same engine as a binary and write a JSON report
go run ./knowledge/cmd/eval \
    --corpus    eval/knowledge/testdata/corpus \
    --golden    eval/knowledge/testdata/golden.jsonl \
    --embedder  qwen:text-embedding-v4 \
    --lanes     bm25,vector,hybrid \
    --out       /tmp/knowledge.json
```

`eval/{locomo,longmemeval}/data/`, `eval/{locomo,longmemeval,history}/results/`
are all excluded by `eval/.gitignore`: the upstream corpora are CC-BY but
bulky, and reports are per-run artifacts.

## Long-running runs: notifications & supervision

LoCoMo10 (~30 min) and especially LongMemEval `_s` / `_m` (10–50 h) outlast
any SSH session, so the runner pushes lifecycle events to Feishu as a
**single live-updated CardKit card** — one chat message per eval run, with
the body rewritten in place on every event (`start`, every
`--notify-progress-pct` percent of ingest + QA, `ingest_done`, `done`, and
a one-shot `error` when QA failure rate exceeds 5 % after 100 questions).

The Feishu **custom-bot webhook** path is intentionally *not* supported:
on a 50 h run it produces hundreds of separate chat messages and floods
the destination group. CardKit is the only sane UX at that timescale.

### 1. Configure a Feishu application

You only need to do this once.

1. Create a self-built app at https://open.feishu.cn/app and note the
   `App ID` (`cli_…`) + `App Secret` (32-hex).
2. Enable the **Bot** ability for the app.
3. Apply for these scopes: `im:chat:readonly`, `im:message`,
   `im:message:send_as_bot`, `cardkit:card`.
4. Publish a version and have a tenant admin approve it.
5. Invite the application bot into the target group chat (right-click
   group → settings → group bot → add bot → pick your app, not a custom
   bot).

### 2. Discover the target chat ID

After the bot is in the group:

```bash
TOKEN=$(curl -s -X POST https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal \
    -H 'Content-Type: application/json' \
    -d "{\"app_id\":\"$FEISHU_APP_ID\",\"app_secret\":\"$FEISHU_APP_SECRET\"}" \
    | jq -r .tenant_access_token)
curl -s -H "Authorization: Bearer $TOKEN" https://open.feishu.cn/open-apis/im/v1/chats | jq .
```

The response's `data.items[].chat_id` (form `oc_…`) is what you want.

### 3. Export credentials and run

```bash
export FEISHU_APP_ID=cli_xxxxxxxxxxxxxxxx
export FEISHU_APP_SECRET=<32-char secret>
export FEISHU_CHAT_ID=oc_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx

GOWORK=off go run ./locomo/cmd/eval \
    --dataset             eval/locomo/data/locomo10.jsonl \
    --notify-name         locomo10-nightly \
    --notify-progress-pct 25 \
    --out                 /tmp/locomo.json
```

`--notify-name` is shown in the card's title; `--notify-progress-pct`
controls milestone resolution (0 disables intermediate updates but
`start` / `ingest_done` / `done` always fire). `--notify-dry-run` routes
events to stderr instead of Feishu, useful for CI smoke tests without
credentials.

### 4. Optional: wrap with the process supervisor

`eval/scripts/run-eval.sh` is a thin shell wrapper that adds the
process-level guarantees Go can't do from inside its own process:
PID-file (prevents concurrent runs of the same name), disk pre-flight,
log tee, and a 30-min log-idle watchdog. It does **not** touch Feishu
itself — all chat notifications come from the binary.

```bash
eval/scripts/run-eval.sh lme-oracle -- \
    /root/bin/eval-locomo \
        --dataset eval/longmemeval/data/longmemeval_oracle.jsonl \
        --notify-name lme-oracle \
        ...
```

`STUCK_AFTER=1800` (30 min) and `DISK_MAX_PCT=90` are the defaults; both
are env-tunable.

## CI integration

- **PR gate**: `make eval` (i.e. `cd eval && GOWORK=off go test ./... -count=1`)
  runs against the synthetic dataset and needs no API keys. A dedicated
  `test-eval` job in `.github/workflows/ci.yml` is wired into the
  `ci-pass` gate.
- **Nightly**: full LoCoMo10 + history compactor (secret-gated; TBD).

## History

`eval/locomo` was previously at `bench/locomo`; `eval/history` at
`bench/history-compression`; `eval/knowledge` at
`tests/quality/knowledge`. The move to `eval/` aligns naming with the
AI/ML evaluation convention (separate from Go's `Benchmark*`
performance benchmarks) and promotes `dataset/` and `metrics/` to the
top level so LoCoMo, LongMemEval, and history can share them.
