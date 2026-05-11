# eval/taubench

Go-native re-implementation of [τ-bench](https://arxiv.org/abs/2406.12045)
single-turn instruction variant — measures an agent's ability to
chain tool calls and mutate a world state to satisfy a customer
goal.

## Why τ-bench

| Suite | Tests |
|-------|-------|
| `eval/locomo`, `eval/longmemeval` | memory recall under a known context |
| `eval/history` | prompt-compaction quality vs token cost |
| `eval/knowledge`, `eval/beir` | retrieval (BM25 / vector / hybrid) |
| `eval/simpleqa` | factual ceiling + calibration |
| **`eval/taubench`** | **tool use** — the capability the agent stack is *for* |

Tool use is the only benchmark in this list whose passing requires the
full FlowCraft agent loop: read intent → invoke a Go tool with the
right structured arguments → ingest the tool result → potentially
chain more calls → confirm in natural language.

## NOT a PR gate

τ-bench is the most LLM-expensive suite in `eval/`. The multi-turn
flavour spends roughly

    per_task_calls ≈ MaxConversationTurns × (1 customer + 1-3 agent)

so a 100-task retail run with the published parameters cuts ~3 000
LLM completions. This is a **periodic regression** (weekly, release
gate, model swap), NOT a per-PR check. CI should at most run a
`--limit 5 --domain retail` smoke that excludes multi-turn tasks; the
full pack belongs in scheduled jobs.

## What we ship today

- **Single-shot** harness — Task.Instruction → agent tool loop → done.
- **Multi-turn dialog** harness — Task.CustomerScenario + Options.CustomerLLM
  → customer ↔ agent exchange until customer emits `###STOP###` or
  the conversation cap is hit. The customer NEVER sees tool calls or
  tool results; only the agent's natural-language utterances.
- **Retail-mini** dataset (7 tasks): cancel-pending / cancel-processing /
  update-shipping / search / cannot-cancel-delivered + 2 multi-turn
  (forgot-order-id; refuse-delivered-politely).
- **Airline-mini** dataset (5 tasks): cancel / update-baggage /
  search-info / refuse-departed + 1 multi-turn (cancel-via-lookup).
- **Retail tools** (6): `get_order`, `cancel_order`, `update_shipping`,
  `list_orders_for_customer`, `get_product`, `search_products`.
- **Airline tools** (7): `get_user`, `get_reservation`,
  `list_user_reservations`, `cancel_reservation`, `update_baggage`,
  `search_flight`, `get_flight`.
- `--domain all` merges retail + airline into one Report so a single
  invocation covers both stacks.
- **State checker** with dot-path predicates and RequiredTools.
- CardKit-driven event stream and JSON Report shape consistent with
  every other suite under `eval/`.

## Quick start

```bash
export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
export FLOWCRAFT_AZURE='{"api_key":"...","model":"gpt-5","base_url":"..."}'

cd eval
# Single-shot-only smoke (no CustomerLLM needed).
GOWORK=off go run ./taubench/cmd/eval \
    --agent-llm qwen:qwen-max \
    --limit     5 \
    --out       /tmp/taubench-singleshot.json

# Full mini-pack including multi-turn tasks.
GOWORK=off go run ./taubench/cmd/eval \
    --agent-llm              qwen:qwen-max \
    --customer-llm           azure \
    --max-conversation-turns 10 \
    --max-agent-turns        12 \
    --out                    /tmp/taubench-multiturn.json
```

The customer LLM picks the **calibration** of the eval: a strong
roleplaying customer (gpt-5 / claude-opus / qwen-max) stays in
character and tests the agent honestly; a weak customer can leak the
answer or give up early, which inflates or deflates the pass rate
unpredictably. Pick a tier comparable to the agent under test.

## Upstream τ-bench task JSON

The bundled `*-mini` datasets are hand-curated regression fixtures.
For numbers comparable to the published τ-bench leaderboard, load the
official task JSON (≈115 retail + ≈50 airline) directly:

```bash
# 1. Vendor the upstream fixtures (sierra-research/tau-bench) somewhere local.
git clone https://github.com/sierra-research/tau-bench /tmp/tau-bench

# 2. Point the CLI at the JSON pair. --domain selects the tool registry;
#    --upstream-tasks + --upstream-initial-state takes over from the
#    bundled mini dataset.
GOWORK=off go run ./taubench/cmd/eval \
    --agent-llm              qwen:qwen-max \
    --customer-llm           azure \
    --domain                 retail \
    --upstream-tasks         /tmp/tau-bench/tau_bench/envs/retail/tasks.json \
    --upstream-initial-state /tmp/tau-bench/tau_bench/envs/retail/data.json \
    --out                    /tmp/taubench-retail-full.json
```

Scoring: for every upstream task we **shadow-execute** the `actions`
list (the gold trace) against a clone of the initial state and pin
the resulting State as the task's `ExpectedFinalState`. The agent
under test passes when its post-run State deep-equals that snapshot
AND every fragment from the upstream `outputs` array appears in its
final reply (case-insensitive substring match).

If the upstream JSON references a tool name that isn't in our
registry, `LoadUpstreamTasks` fails LOUDLY with the offending action
index — fix the registry or fix the JSON, never silently skip.

### Schema assumptions

The loader currently expects the schema documented inline at
`upstream.go::UpstreamTask`. Upstream `tau-bench` has shifted column
names across revisions; when porting a new commit, audit `UpstreamTask`
against `tau_bench/types.py` and bump the loader rather than mutating
the on-disk JSON.

## Roadmap

- LLM-as-customer multi-turn for upstream tasks: today
  `LoadUpstreamTasks` lifts `instruction` onto `Task.Instruction`
  (single-shot). The upstream "user persona" is roughly equivalent
  to our `CustomerScenario` — a later commit can choose the form
  per-task or behind a flag.
- Confirmation-number extraction: the upstream `outputs` array
  occasionally encodes a "score = LLM judgment of agent reply"
  rubric; ours uses strict substring match for portability. A
  judge-LLM mode could be added when the heuristic produces too many
  false negatives.

## Quick start

```bash
export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'

cd eval
GOWORK=off go run ./taubench/cmd/eval \
    --agent-llm qwen:qwen-max \
    --max-turns 12 \
    --out       /tmp/taubench-qwenmax.json
```

Sample stderr summary (multi-turn pack):

```
  total=7  passed=6  pass_rate=0.857  duration=51284ms
    retail     pass_rate=0.857 (6/7)
    [PASS] retail-cancel-pending             mode=single-shot agent_turns=2 customer_turns=0 tools=1
    [PASS] retail-cancel-processing          mode=single-shot agent_turns=2 customer_turns=0 tools=1
    [PASS] retail-update-shipping            mode=single-shot agent_turns=2 customer_turns=0 tools=1
    [FAIL] retail-cannot-cancel-delivered    mode=single-shot agent_turns=3 customer_turns=0 tools=2 state mismatch: ...
    [PASS] retail-product-search             mode=single-shot agent_turns=2 customer_turns=0 tools=1
    [PASS] retail-dialog-forgot-order-id     mode=multi-turn  agent_turns=4 customer_turns=2 tools=2
    [PASS] retail-dialog-refuse-delivered    mode=multi-turn  agent_turns=2 customer_turns=2 tools=1
```

## Authoring new tasks

A task is a single struct literal:

```go
{
    ID:           "retail-update-shipping-bulk",
    Domain:       "retail",
    Instruction:  "Hi, I'm CUST-1. Please change the shipping address on every pending order to 22 Engineering Way.",
    InitialState: baseState(),
    Expected: ExpectedOutcome{
        StateChecks: []StateCheck{
            {Path: "orders.ORD-1001.shipping_address", Equals: "22 Engineering Way"},
        },
        RequiredTools: []string{"list_orders_for_customer", "update_shipping"},
    },
},
```

Tools live in `retail.go` next to the dataset; add a new domain by
mirroring the `NewRetailTools()` + `NewXxxMiniDataset()` pair.

## Failure modes

The harness reports three distinct failure shapes so the `Reason`
string on each `TaskResult` is actionable:

| Reason prefix | Meaning |
|---------------|---------|
| `state mismatch: ...` | Agent finished but the world wasn't mutated correctly. |
| `required tools never called: [...]` | Agent finished without calling a tool the fixture demanded. |
| `agent did not finish within N turns` | Hit `--max-agent-turns`; agent kept calling tools forever. |
| `conversation did not terminate within N turns` | Multi-turn task: customer never emitted `###STOP###` and hit `--max-conversation-turns`. |
| `customer LLM error: ...` | Customer-side provider error; usually a credential issue. |
| `agent LLM error: ...` | Upstream provider error; check the model spec. |
