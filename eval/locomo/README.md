# eval/locomo

Evaluation scaffold for `memory/recall` against the LoCoMo / LongMemEval benchmarks.

## Quick start

```bash
# 1) bundled synthetic dataset (no network, no LLM)
cd eval && GOWORK=off go run ./cmd/eval locomo run --dataset synthetic --out results/synthetic.json

# 2) compare against a previous report
GOWORK=off go run ./cmd/eval locomo compare results/baseline.json results/synthetic.json

# 3) (optional) external datasets — see fetch instructions
GOWORK=off go run ./cmd/eval locomo fetch

# 4) full LoCoMo10 (10 conversations, 1542 questions, ~1m no-LLM run)
git clone https://github.com/snap-research/locomo eval/locomo/data/locomo
GOWORK=off go run ./cmd/eval locomo convert \
    --in  eval/locomo/data/locomo/data/locomo10.json \
    --out eval/locomo/data/locomo10.jsonl
GOWORK=off go run ./cmd/eval locomo run \
    --dataset eval/locomo/data/locomo10.jsonl \
    --out     eval/locomo/results/locomo10.json
```

> `eval/locomo/data/` and `eval/locomo/results/` are git-ignored: the
> upstream dataset is CC-BY but bulky, and report files are per-run artifacts.

## Layout

```
eval/locomo/
├── runners/         Runner interface (one per memory backend)
│   └── flowcraft/   default in-memory retrieval Index runner
├── cli.go           Cobra wiring for `eval locomo run / fetch / compare`
├── cli_convert.go   converter sub-subcommand `eval locomo convert`
└── cli_ingest.go    warm-up sub-subcommand `eval locomo ingest`
```

The `dataset/` and `metrics/` helpers live one level up at `eval/dataset/` and
`eval/metrics/` and are shared with `eval/history/`.

## Adding a backend

Implement `runners.Runner` in a new package under `runners/<name>/` and wire
it into the `cmd/eval` flag handler. Reuse `eval/dataset` and `eval/metrics` —
no other change is needed.

## CI policy

- Pull requests run the hermetic synthetic suites. LLM-backed comparisons
  self-skip unless credentials and providers are configured explicitly.
- Pending `sdk`, `memory`, or `sdkx` releases rerun the hermetic eval suite
  before tags may be created.
- Full LoCoMo and judge-backed quality runs remain opt-in and are not claimed as
  an automated release gate.
