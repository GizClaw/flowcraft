# bench/locomo

Evaluation scaffold for `sdk/memory/ltm` against the LoCoMo / LongMemEval benchmarks.

## Quick start

```bash
# 1) bundled synthetic dataset (no network, no LLM)
go run ./bench/locomo/cmd/eval --dataset synthetic --out results/synthetic.json

# 2) compare against a previous report
go run ./bench/locomo/cmd/compare results/baseline.json results/synthetic.json

# 3) (optional) external datasets — see fetch instructions
go run ./bench/locomo/cmd/fetch

# 4) full LoCoMo10 (10 conversations, 1542 questions, ~1m no-LLM run)
git clone https://github.com/snap-research/locomo bench/locomo/data/locomo
go run ./bench/locomo/cmd/convert-locomo \
    -in  bench/locomo/data/locomo/data/locomo10.json \
    -out bench/locomo/data/locomo10.jsonl
go run ./bench/locomo/cmd/eval \
    --dataset bench/locomo/data/locomo10.jsonl \
    --out     bench/locomo/results/locomo10.json
```

> `bench/locomo/data/` and `bench/locomo/results/` are git-ignored: the
> upstream dataset is CC-BY but bulky, and report files are per-run artifacts.

## Layout

```
bench/locomo/
├── dataset/   schema + bundled synthetic loader
├── runners/   Runner interface (one per memory backend)
│   └── flowcraft/   default in-memory retrieval Index runner
├── metrics/   EM / F1 / LLM-as-judge / latency aggregator
└── cmd/
    ├── fetch/    download instructions for LoCoMo / LongMemEval
    ├── ingest/   warm up runner with a dataset's conversations
    ├── eval/     ingest + Q&A loop + report
    └── compare/  markdown diff between two reports
```

## Adding a backend

Implement `runners.Runner` in a new package under `runners/<name>/` and wire
it into the `cmd/eval` flag handler. Reuse `dataset.*` and `metrics.*` —
no other change is needed.

## CI policy

- **PR must run** synthetic + LongMemEval-50 (~3 min, ~$0.5/run);
  `qa.judge` regression > 2pp or `latency.save.p95` > +30% fails CI.
- **Nightly** runs full LoCoMo (~30 min, ~$5/run); results archived under
  `results/nightly/<date>.json`.
- **Release-gate** compares against the previous tagged release baseline.
