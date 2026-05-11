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

## What we ship today

- **Retail mini** task pack (5 hand-curated tasks) baked into the
  binary so a smoke run needs no external assets.
- **Retail tools** (`get_order`, `cancel_order`, `update_shipping`,
  `list_orders_for_customer`, `get_product`, `search_products`).
- **State checker** with dot-path predicates (`orders.ORD-1001.status == "cancelled"`).
- Single-turn instruction harness (the customer's full goal goes in
  one user message; the agent then chains tool calls until done).
- CardKit-driven event stream and JSON Report shape consistent with
  every other suite under `eval/`.

## Roadmap

- LLM-as-customer multi-turn dialog harness.
- Airline-domain tools and tasks.
- Converter for the upstream τ-bench task JSON so the official
  retail (≈115) and airline (≈50) sets can be run unmodified.

## Quick start

```bash
export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'

cd eval
GOWORK=off go run ./taubench/cmd/eval \
    --agent-llm qwen:qwen-max \
    --max-turns 12 \
    --out       /tmp/taubench-qwenmax.json
```

Sample stderr summary:

```
  total=5  passed=4  pass_rate=0.800  duration=18743ms
    retail     pass_rate=0.800 (4/5)
    [PASS] retail-cancel-pending             turns=2 tools=1
    [PASS] retail-cancel-processing          turns=2 tools=1
    [PASS] retail-update-shipping            turns=2 tools=1
    [FAIL] retail-cannot-cancel-delivered    turns=3 tools=2 state mismatch: ...
    [PASS] retail-product-search             turns=2 tools=1
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
| `agent did not finish within N turns` | Hit `--max-turns`; agent kept calling tools forever. |
| `agent LLM error: ...` | Upstream provider error; check the model spec. |
