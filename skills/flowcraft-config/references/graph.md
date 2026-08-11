# Graph definition JSON

The graph engine (`engine.kind: graph`) takes `settings.graph` as a
config source: literal content, `{file: ...}`, or `{embed: ...}`, JSON or
YAML. File definitions are limited to 1 MiB.

```json
{
  "name": "assistant",
  "version": "1",
  "entry": "chat",
  "nodes": [
    {"id": "chat", "type": "inference", "config": {...}}
  ],
  "edges": [
    {"from": "chat", "to": "tools", "condition": "tool_pending == true"},
    {"from": "tools", "to": "chat"},
    {"from": "chat", "to": "__end__"}
  ]
}
```

- `entry` must name a node; ids unique; edges target a node or `__end__`.
- `condition`/`skip_condition` are expr-lang expressions over board vars;
  `${board.<name>}` references inside config string values resolve before
  node decode. `__iterations` is exposed in edge conditions for loops.
- Multiple outgoing edges may fire (fan-out); zero firing edges end the
  branch.
- Bump `version` when node semantics change; it is recorded in checkpoints
  and compared on resume.

## Node types and configs

### inference

```json
{
  "id": "chat",
  "type": "inference",
  "config": {
    "model": {"id": {"provider": "deepseek", "name": "deepseek-v4-flash"}},
    "messages_channel": "main",
    "system_prompt": "You are a helpful assistant.",
    "output_key": "reply",
    "usage_key": "usage",
    "tool_pending_key": "tool_pending",
    "stream": true,
    "tools": ["web_search"],
    "all_tools": false,
    "tool_choice": {"type": "auto"},
    "temperature": 0.4,
    "top_p": 1,
    "max_output_tokens": 1024,
    "reasoning_enabled": true,
    "reasoning_effort": "high",
    "extensions": [{"provider": "qwen", "id": "thinking_budget", "fields": {"budget": 4096}}]
  }
}
```

`model` is a `ModelRef`: the nested `id` object is required — writing
`model: {provider: ..., name: ...}` without `id` is a common error. The
node appends one assistant message to the channel and sets
`tool_pending_key` when finish reason is tool calls; it never executes
tool calls itself. `system_prompt` accepts `{file: ...}` refs (materialized
by the graph factory).

### tool

```json
{
  "id": "tools",
  "type": "tool",
  "config": {
    "messages_channel": "main",
    "results_key": "tool_results"
  }
}
```

Executes the channel tail's tool calls as one batch and appends one
role=tool message. Allow-listing lives in the dispatcher middleware, not
the node.

### script

```json
{
  "id": "tally",
  "type": "script",
  "config": {
    "runtime": "js",
    "name": "tally",
    "source": {"file": "./scripts/tally.js"},
    "config": {"max": 10}
  }
}
```

`runtime` and `source` are required; `source` accepts inline text or
`{file: ...}`. Standard bridges: `board`, `expr`, `host`, `run`, `tools`,
`inference`, `node`, `stream`, `parallel`; `fs` and `shell` globals are
opt-in via wired workspace/sandbox deps.

## Engine settings

```yaml
engine:
  kind: graph
  settings:
    graph: {file: ./graphs/assistant.json}
    script_runtime_name: js      # default js
    build:
      max_iterations: 100
      timeout: 5m
      run_end_publish_timeout: 5s
      max_node_retries: 2
      parallel:
        enabled: true
        branch_timeout: 30s
        max_concurrency: 8
        max_branches: 32
        merge_strategy: last_write_wins   # or first_write_wins
```

`run_end_publish_timeout` must be positive. Deps required by node types:
`inference` → whole `inference.Assembly`; `tool` nodes / inference tools →
`tool.Assembly`; scripts with fs → `workspace.Workspace` item; scripts with
shell → `sandbox.Runner` item; script nodes → `agent.ScriptRuntime`.

## Sources of truth

`docs/guides/graph.md`, `sdk/graph/definition.go`,
`sdk/graph/nodes/{inference,tool}.go`, `sdk/graph/nodes/script/node.go`.
