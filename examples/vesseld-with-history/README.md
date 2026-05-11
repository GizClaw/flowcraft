# vesseld with conversational history

Single-vessel, single-agent demo that wires a shared `HistoryStore` so the agent remembers what was said earlier in the conversation. Pair this with [`vesseld-multi-vessel`](../vesseld-multi-vessel/) — that one shows multi-agent + Kanban delegation; this one shows how a Vessel preserves transcript state across turns.

## What it shows

| File | Concept |
|---|---|
| `daemon.yaml` | Daemon-wide config: control socket, drain timeout, shared LLM rate-limit bucket. |
| `shared/openai.yaml` | One `LLMProfile`. |
| `shared/history.yaml` | A `HistoryStore` with `ref: buffer` and a per-conversation cap of 500 messages. |
| `vessels/assistant/vessel.yaml` | `Vessel` references the HistoryStore; the `Agent` opts into `historyAccess: read_write`. |

Multiple Vessels can reference the **same** `HistoryStore` — useful when a "human" vessel and a "supervisor sidecar" vessel need to see one another's turns.

## Prerequisites

```bash
export OPENAI_API_KEY=sk-...
go build -o ./vesseld ./cmd/vesseld
```

## Run

```bash
./vesseld validate --config examples/vesseld-with-history -R
./vesseld run      --config examples/vesseld-with-history -R
```

## Drive a multi-turn conversation

The key is **a stable `context_id`** in each request. Different `context_id`s give independent transcripts; the same one continues a thread.

```bash
SOCK=/tmp/vesseld-with-history.sock
CONV=demo-alice                     # any stable string; treat it as the conversation key

# Turn 1: tell the agent something.
curl --unix-socket $SOCK -X POST http://vesseld/v1/vessels/assistant/call \
  -H 'content-type: application/json' \
  -d "{\"agent\":\"assistant-agent\",\"context_id\":\"$CONV\",\"query\":\"My name is Alice and I prefer coffee over tea.\"}"

# Turn 2: ask a follow-up — the agent recalls from the buffer.
curl --unix-socket $SOCK -X POST http://vesseld/v1/vessels/assistant/call \
  -H 'content-type: application/json' \
  -d "{\"agent\":\"assistant-agent\",\"context_id\":\"$CONV\",\"query\":\"What should I drink this morning?\"}"
```

The second response should reference coffee. Change `CONV` to a new value and ask the same question — the agent will (correctly) admit it doesn't know.

## Caveats — what this is NOT

> **The buffer history lives in the daemon's memory.** Restart `vesseld` and every conversation is gone.
>
> Persistent transcripts across daemon restarts arrive in `v0.2.0` via the `Compacted` history flavor (hierarchical SummaryDAG + SQLite/Postgres-backed `Store`). The declarative YAML schema and the `vessel.WithSessionStore` option are tracked under [`internal-docs/roadmap.md`](../../internal-docs/roadmap.md) §4 v0.2.0.
>
> If you need cross-restart memory **today**, embed `vessel` as a library and pass `history.NewCompacted(store, llm, ws)` directly — see `examples/chatbot-with-recall/` for the in-process pattern.

## Going further

- Set `maxMessages` lower (e.g. 20) and observe the agent "forgetting" once eviction kicks in.
- Add a second agent to the same vessel with `historyAccess: read_only` to model a supervisor that watches without writing.
- Combine with `examples/vesseld-multi-vessel/` patterns: point both demo daemons at the same `HistoryStore` document and they'll share transcripts.
