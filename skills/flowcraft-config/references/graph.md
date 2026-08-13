# Graph definition JSON

The graph engine (`engine.kind: agent.Engine`, `impl: graph`) reads
`settings.graph` as literal content or `{file: ...}` / `{embed: ...}`.
File definitions are capped at 1 MiB.

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

## Rules

- `entry` must name a node; IDs are unique.
- Edges target node IDs or `__end__`.
- `condition`/`skip_condition` are expr-lang expressions over board vars.
- Model refs require the nested `id` form.
- Script nodes require `runtime` and `source`; the bound runtime must match
  the engine's `script_runtime_name`.

## Node types

### inference

Common config fields: `model`, `messages_channel`, `system_prompt`,
`output_key`, `usage_key`, `tool_pending_key`, `stream`, `tools`,
`all_tools`, `temperature`, `max_output_tokens`, `extensions`.

### tool

Executes pending tool calls in one batch and appends one `role=tool` message.

### script

Runs a JS or Lua script. `runtime` names the bound `agent.ScriptRuntime`.

## Sources of truth

`core/graph/definition.go`, `core/graph/nodes/`,
`core/graph/resource/resource.go`, `docs/guides/graph.md`.
