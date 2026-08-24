---
layout: default
title: Memory Stack Guide
---
# Memory Stack Guide

`core/memory` defines the memory capability contracts. Concrete
implementations are app-registered and own their settings schema; this guide
does not reference any specific implementation module.

## Layers

| Layer | Owns | Location |
| --- | --- | --- |
| Contract | `ContextProvider`, `TurnSink`, `DocumentSink`, `ContextRenderer`, `Scope` | `core/memory` |
| Glue | `memory.Assembly`, `memory.context` / `memory.turn` hooks, GoTemplate renderer | `core/memory` |
| Implementation | canonical stores, projections, retrieval, worker, lifecycle | app-registered module |

## Scope

`Scope` hard-partitions memory by runtime, user, and agent:

```go
scope := corememory.Scope{
    RuntimeID: "memories",
    UserID:    "user-1",
    AgentID:   "assistant",
}
```

The effective partition is `RuntimeID + UserID + AgentID`.

## Deployment

Memory implementations are app-registered because their settings schema is
implementation-owned:

```yaml
resources:
  memories:
    kind: memory.Assembly
    impl: mymemory        # app-registered implementation; name is yours
    deps:
      workspace: ws/project
      inference: infer
    settings:
      file: ./memory.yaml
```

Hooks bind the whole assembly as their `memory` dependency.

### `memory.context` seed hook (`hook.prepare`)

Prepares each turn's board with recalled context. The `query` section
must select exactly one source: `literal` text, a board var (`board`),
the current request message (`current_message`), or the recall window
(`recent_only`). Recalled items land in the board var named by `output`,
and an optional renderer writes rendered text to a second board var:

```yaml
agents:
  assistant:
    prepare:
      - type: memory.context
        deps:
          memory: memories
        settings:
          query:
            literal: "relevant prior conversation"  # or board / current_message / recent_only
          scope:
            runtime_id: memories
            user_id: user-1
            agent_id: assistant
          conversation_id: conv-1        # optional; defaults to the request ContextID
          dataset_ids: [docs]            # optional
          budget: {max_tokens: 2000, max_items: 50, max_chars: 8000}  # optional
          min_score: 0.5                 # optional; [0, 1]
          output: memory_items           # required; non-reserved board var
          render:
            output: memory_text          # must differ from output
            gotmpl: {template: "{{ range .Items }}{{ contentText .Content }}\n{{ end }}", max_chars: 8000}
```

`scope` hard-partitions recall by `runtime_id` + `user_id` + `agent_id`
(see Scope above). `output` (and `render.output`) must be a non-reserved
board variable name — the `__` prefix is reserved for the engine.

### `memory.turn` commit hook (`hook.commit`)

Pushes each completed turn's channel into the assembly as durable memory:

```yaml
agents:
  assistant:
    commit:
      - type: memory.turn
        deps:
          memory: memories
        settings:
          scope:
            runtime_id: memories
            user_id: user-1
            agent_id: assistant
          conversation_id: conv-1   # optional; defaults to the request ContextID
          channel: __main_channel   # optional; defaults to the main channel
```

Committed turns are idempotent per run id, so retried turns do not
duplicate memory.

See [deploy.md](deploy.md) and [resource.md](resource.md).
