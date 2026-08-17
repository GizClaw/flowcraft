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

- `entry` must name a node; IDs are unique — `--type graph` **validator**.
- Edges target node IDs or `__end__` — `--type graph` **validator**.
- `condition`/`skip_condition` are expr-lang expressions over board vars —
  compiled by the host build, not checked by the validator.
- Model refs require the nested `id` form — node config decode at host
  build.
- Script nodes require `runtime` and `source`; the bound runtime must match
  the engine's `script_runtime_name` — host build.

The validator (`--type graph`) runs `GraphDefinition.Validate` only:
structure, node id uniqueness, entry presence, and edge endpoints. Node
configs are not decoded, node types are not checked against a registry,
and expressions are not compiled; those checks happen when the host
builds the graph engine.

## Engine settings (`agent.Engine/graph`)

| Key | Meaning |
| --- | --- |
| `graph` | graph definition: literal content, `{file: ...}`, or `{embed: ...}` (required) |
| `script_runtime_name` | name of the `agent.ScriptRuntime` dep bound to script nodes; default `js` |
| `build.max_iterations` | cap on total node invocations per run |
| `build.timeout` | wall-clock bound for one execute call (e.g. `1h`) |
| `build.run_end_publish_timeout` | deadline for run-end lifecycle events |
| `build.max_node_retries` | per-node retries before the run fails |
| `build.parallel.enabled` | enable parallel waves |
| `build.parallel.branch_timeout` | per-branch wall-clock bound |
| `build.parallel.max_concurrency` | max concurrent branches |
| `build.parallel.max_branches` | max total branches per wave |
| `build.parallel.merge_strategy` | `first_write_wins` or `last_write_wins` |

Engine deps are derived from the definition: inference needs `inference`
(explicit `model`) and/or `router` (no `model`), tool nodes need `tools`,
script nodes need `script_runtime`. `workspace` and `sandbox` are optional
and unlock the script `fs` / `shell` globals.

## Node types

### `inference` — single-shot LLM generation

Runs one `Generate` call; the channel tail is the current input (role
`user` or `tool`), and the assistant message (tool calls included) is
appended to the same channel. The node never executes tool calls — a
`tool_calls` finish reason is flagged onto `tool_pending_key` for edges.

| Config field | Meaning |
| --- | --- |
| `model` | explicit target `{id: {provider, name}, profile?}`; absent defers to the wired router |
| `messages_channel` | board channel holding the conversation; empty means the main channel |
| `system_prompt` | prepended system message when the context does not start with one (may be `{file: ...}` / `{embed: ...}`) |
| `output_key` / `usage_key` / `tool_pending_key` | board vars receiving the message / usage / tool-pending flag |
| `stream` | stream deltas incrementally; the board still gets one assembled message |
| `tools` / `all_tools` | named catalog tools, or the catalog's entire visible set |
| `tool_choice` | constrain when/which tools are called |
| `temperature` / `top_p` / `max_output_tokens` | sampling knobs |
| `reasoning_enabled` / `reasoning_effort` | reasoning switch / depth |
| `response_format` | `text`, `json_object`, or `json_schema` (re-validated) |
| `extensions` | provider knobs `{provider, id, fields}` |

### `tool` — batch tool execution

Reads the `tool_call` parts off the channel tail's assistant message,
executes the whole batch through the tool dispatcher (call ids preserved),
and appends the results as one `role=tool` message.

| Config field | Meaning |
| --- | --- |
| `messages_channel` | channel whose tail holds pending tool calls; empty means the main channel |
| `results_key` | board var receiving the raw `[]message.Result` |

Allow-listing/approval policy lives in the dispatcher's middleware, not the
node.

### `script` — embedded script with host bindings

Runs inline JS or Lua on the bound `agent.ScriptRuntime`. Decoding is
strict: unknown top-level config fields are errors.

| Config field | Meaning |
| --- | --- |
| `runtime` | name of the wired `agent.ScriptRuntime`; must equal the engine's `script_runtime_name` (required) |
| `source` | inline script source; required (may be `{file: ...}` / `{embed: ...}`) |
| `name` | execution label for errors/runtime pooling; defaults to the node ID |
| `config` | becomes the script's `config` global; values may carry `${board.*}` references |

## Script bridges

Scripts get named globals through `core/agent/bindings` (plus three
graph-layer bridges). Availability depends on engine deps:

| Global | Wired when | Provides |
| --- | --- | --- |
| `board` | always | read/write vars and channels |
| `expr` | always | eval expr-lang expressions |
| `host` | always | publish / emit / interrupt / askUser / usage |
| `run` | always | read-only run identity |
| `node` | always | current graph node identity |
| `tools` | `tools` dep | call tools / list catalog |
| `inference` | `inference` and/or `router` dep | LLM generation / routing / streaming |
| `stream` | always (host with event bus) | subscribe to node stream deltas |
| `parallel` | always (during parallel waves) | cancel sibling branches |
| `fs` | `workspace` dep | workspace file ops |
| `shell` | `sandbox` dep | sandboxed command execution |
| `runtime` | always | nested sub-script execution |

### `board`

- `getVar(key)`, `setVar(key, value)`, `deleteVar(key)`, `getVars()`,
  `hasVar(key)`
- `channel(name)` → message objects (never null); `setChannel(name, msgs)`,
  `appendChannel(name, msg)` throw on validation errors
- `MAIN_CHANNEL` — the reserved default channel constant

Messages use the inference wire format `{role, content: {parts: [...]}}`;
decoding is strict, so typos surface as errors.

### `expr`

`expr.eval(expression, env)` — evaluate expr-lang against an environment
map.

### `host`

`host.publish(subject, payload)`, `host.emit(type, payload)`,
`host.checkInterrupt()` → `{cause, detail} | null`,
`host.askUser({parts, schema, source, metadata})` →
`{parts, metadata}`, `host.reportUsage({input, output, total})`.
Every method returns nil/`""` on a no-op host.

`host.emit` event types in the graph script node: `token` (text delta),
`tool_call` (`{id, name, arguments}`), `tool_result`
(`{tool_call_id, content, is_error}`), `part` (canonical part wire object),
or a passthrough type. Emission is fire-and-forget.

### `run`

`get_run_id()`, `get_task_id()`, `get_agent_id()`, `get_context_id()`,
`get_parent_run_id()` — all return `""` when unset.

### `node`

`node.id()` (the node's ID), `node.type()` (the registered type name).

### `tools`

- `call(name, argumentsJSON)` → `{content, is_error, tool_call_id}`
- `callAll([{name, arguments, id?}, ...])` → per-entry result plus `name`;
  model-issued `id`s are forwarded verbatim
- `list()` → allowed tool names; `definitions()` → wire-ready tool JSON

By default **no** tool is callable until the host allow-lists it; denied
entries become `is_error` results.

### `inference`

Requests/responses are the canonical `GenerateRequest` / `GenerateResponse`
wire JSON; the bridge performs one call per invocation, so multi-turn tool
loops live in script-land (`tools.callAll`, then `input.role = "tool"`).

- `generate(request)` — exact model (`model` key required)
- `route(request)` — router-selected target (no `model`); response gains
  `trace`
- `explain(request)` / `routeExplain(request)` — provider-less preflight
  (exact model vs router selection); the route twin returns
  `{explanation, decision, limits}`
- `models()` / `inspect(model)` — model catalog / one descriptor
- `embed(request)` / `routeEmbed(request)` — embedding twins of
  generate/route
- `explainEmbed(request)` / `routeExplainEmbed(request)` — embedding twins
  of explain/routeExplain
- `explainStream(request)` / `routeExplainStream(request)` — stream
  preflight without opening a provider stream
- `stream(request)` / `routeStream(request)` — streaming twins returning
  `{next, result, close}`

Extensions ride an `extensions` array of `{provider, id, fields}`, resolved
through host-registered decoders.

### `stream`

`stream.subscribe_node({node_id | node_ids, run_id?, buffer_size?})` →
`{next, next_timeout_ms, current, close}`. `node_id`/`node_ids` are
mutually exclusive; `run_id` must match the current run; `buffer_size` is
1..4096 (default 256, `DropOldest`). Events are maps with `event`
(`step.started` / `step.ended` / `step.skipped` / `stream.delta`),
`envelope_id`, `id`, `subject`, `time`, `run_id`, `node_id`, `agent_id`.
The iterator closes when every subscribed node has terminated or the
invocation ends.

### `parallel`

`parallel.cancelNode(nodeID, reason)` → `bool`; false outside a parallel
wave.

### `fs`

`fs.read(path)` → `(string, error)`, `fs.write(path, content)`,
`fs.exists(path)` → `bool`, `fs.delete(path)`.

### `shell`

`shell.exec(cmd, args...)` → `{exit_code, stdout, stderr}` (no throw on
non-zero exits; a rejected command returns `exit_code: -1`).

### `runtime`

`runtime.execScript(source, config)` — nested sub-script with inherited
bindings; degrades to a `not_available` signal when the runtime cannot
nest.

## Sources of truth

`core/graph/definition.go`, `core/graph/nodes/inference.go`,
`core/graph/nodes/tool.go`, `core/graph/nodes/script/`,
`core/graph/resource/resource.go`, `core/agent/bindings/`,
`docs/guides/graph.md`.
