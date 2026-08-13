---
layout: default
title: Graph Runtime
---
# Graph Runtime Guide

`core/graph` is the built-in declarative DAG engine. It compiles a JSON or
YAML graph definition into an `agent.Engine`, then runs waves over an
`agent.Board`.

## Layers

| Layer | Type | Job |
| --- | --- | --- |
| Wire | `GraphDefinition` | serializable graph document |
| Registration | `Registry` + node types | bind node type names to handlers |
| Build | `Build(def, reg, opts...)` | validate and compile edges/conditions |
| Execution | `*Graph` | run the frontier wave by wave |

A `*Graph` is an `agent.Engine`.

## Definition

```json
{
  "name": "assistant",
  "version": "1",
  "entry": "chat",
  "nodes": [
    {"id": "chat", "type": "inference", "config": {
      "model": {"id": {"provider": "deepseek", "name": "deepseek-v4-flash"}},
      "messages_channel": "main"
    }},
    {"id": "tools", "type": "tool", "config": {
      "messages_channel": "main",
      "results_key": "tool_results"
    }}
  ],
  "edges": [
    {"from": "chat", "to": "tools", "condition": "tool_pending == true"},
    {"from": "tools", "to": "chat"},
    {"from": "chat", "to": "__end__"}
  ]
}
```

`condition` and `skip_condition` use expr-lang expressions over board
variables. `${board.<name>}` references inside config strings resolve before
node decode.

## Node types

- `inference` runs model inference and writes results/usage/tool pending keys.
- `tool` executes pending tool calls in one batch.
- `script` runs a JS or Lua script bound to an `agent.ScriptRuntime`.

Node factories are registered through `core/graph/nodes` and
`core/graph/nodes/script`. The deployment-facing engine factory is
`core/graph/resource`.

See [deploy.md](deploy.md) for the `agent.Engine/graph` resource binding.
