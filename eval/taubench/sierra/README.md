# Sierra τ-bench fixtures

This directory bridges between the published
[`sierra-research/tau-bench`](https://github.com/sierra-research/tau-bench)
test set (Python `Task(...)` literals + per-domain `data/*.json`) and
the JSON shape `eval/taubench` consumes.

Sierra's data is CC-BY licensed but bulky (~210k lines of JSON for the
retail + airline test sets, ~165 gold task traces). It is **not**
committed to this repo; an operator stages it once per Sierra release
on the runner host before dispatching `eval-taubench.yml` with
`--domain sierra-retail` or `--domain sierra-airline`.

## One-time staging

```bash
# 1. clone Sierra at the commit you want to evaluate against. Record
#    the commit hash in the leaderboard so the comparison is
#    reproducible.
cd /tmp && git clone https://github.com/sierra-research/tau-bench.git
cd tau-bench && git rev-parse HEAD       # write this to leaderboard.md

# 2. install pydantic (Sierra's Task / Action live in tau_bench/types.py
#    and need pydantic to import). Nothing else from Sierra's Python
#    stack is needed — prep.py bypasses the top-level __init__
#    chain that pulls in litellm.
pip3 install --user pydantic

# 3. run prep.py from a flowcraft checkout. It writes per-domain
#    initial_state.json (Sierra data/*.json merged under top-level
#    keys) + tasks_test.json (the UpstreamTask wire shape consumed
#    by eval/taubench/upstream.go LoadUpstreamTasks).
cd /path/to/flowcraft
python3 eval/taubench/sierra/prep.py \
    --sierra /tmp/tau-bench \
    --out    /var/lib/flowcraft-eval/datasets/taubench

# 4. verify on the runner host (ssh flowcraft-eval):
ls /var/lib/flowcraft-eval/datasets/taubench/{retail,airline}/
# retail/initial_state.json  retail/tasks_test.json   (~3.6 MB, 115 tasks)
# airline/initial_state.json airline/tasks_test.json  (~5.0 MB, 50 tasks)
```

## Dispatching the workflow

```bash
gh workflow run eval-taubench.yml --ref main \
    -f domain=sierra-retail \
    -f agent_llm=azure_4o \
    -f customer_llm=azure_4o \
    -f upstream_tasks=/var/lib/flowcraft-eval/datasets/taubench/retail/tasks_test.json \
    -f upstream_initial_state=/var/lib/flowcraft-eval/datasets/taubench/retail/initial_state.json \
    -f concurrency=4 \
    -f per_task_timeout=5m
```

Same flags for airline; substitute `sierra-airline` and the airline
dataset paths.

## What this is comparable to

The `eval/taubench` Go harness's metrics
([pass@1, per-domain pass-rate](../../leaderboard.md)) on
`--domain sierra-retail` / `--domain sierra-airline` runs are the
**right** baselines to cite against the Sierra leaderboard rows
("TC + gpt-4o" 0.604 / 0.420, "TC + claude-3-5-sonnet-20241022"
0.692 / 0.460). The bundled mini-pack runs
(`--domain retail | airline`, 5+7 hand-curated tasks) are not — they
are first-cut harness smoke checks, useful only to assert the LLM
wiring + the Tool / Handler contracts work end-to-end.

## Smoke test (no LLM cost)

`go test -run TestSierraRetailShadowRun_AllTasks ./eval/taubench/...`
loads every staged task and shadow-runs its gold action trace against
a clone of `initial_state.json`. A failure here means the Go port of
some Sierra tool drifted (wrong kwargs handling, wrong state
mutation, wrong error string) — every gold trace in the published
Sierra fixtures executes deterministically against Sierra's own
harness by construction, so any divergence is the Go side's bug.

The test is gated on `FLOWCRAFT_TAUBENCH_SIERRA_DATA` pointing at the
staged root (e.g. `/var/lib/flowcraft-eval/datasets/taubench`); set
it locally to verify after re-staging.

## Pinning Sierra revisions

`tau-bench` evolves — task wordings change, state schemas drift. If
this directory's tools diverge from the staged fixtures (smoke test
errors out), pick one of:

- Re-pin to the same Sierra commit recorded last in
  `eval/leaderboard.md` (preferred when comparing to published numbers).
- Re-run `prep.py` against the new Sierra HEAD, fix the affected tool
  in `sierra_retail.go` / `sierra_airline.go`, and bump the leaderboard
  Sierra-commit footnote.

`prep.py` does not validate Sierra's shape against our tool set; the
smoke test does. Run it after every restage.
