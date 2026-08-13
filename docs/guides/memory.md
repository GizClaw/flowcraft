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
    impl: flowcraft
    deps:
      workspace: ws/project
      inference: infer
    settings:
      file: ./memory.yaml
```

Hooks bind the whole assembly as their `memory` dependency.

See [deploy.md](deploy.md) and [resource.md](resource.md).
