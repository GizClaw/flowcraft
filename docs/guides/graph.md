---
layout: default
title: Graph Runtime
---
# Graph Runtime Guide

`core/graph` is the built-in declarative DAG engine. It compiles a JSON or
YAML graph definition into an `agent.Engine`, then runs waves over an
`agent.Board`.

## Layers

| Layer        | Type                       | Job                                   |
| ------------ | -------------------------- | ------------------------------------- |
| Wire         | `GraphDefinition`          | serializable graph document           |
| Registration | `Registry` + node types    | bind node type names to handlers      |
| Build        | `Build(def, reg, opts...)` | validate and compile edges/conditions |
| Execution    | `*Graph`                   | run the frontier wave by wave         |

A `*Graph` is an `agent.Engine`.

## Definition

```json
{
  "name": "assistant",
  "version": "1",
  "entry": "chat",
  "nodes": [
    {
      "id": "chat",
      "type": "inference",
      "config": {
        "model": {
          "id": { "provider": "deepseek", "name": "deepseek-v4-flash" }
        },
        "messages_channel": "main"
      }
    },
    {
      "id": "tools",
      "type": "tool",
      "config": {
        "messages_channel": "main",
        "results_key": "tool_results"
      }
    }
  ],
  "edges": [
    { "from": "chat", "to": "tools", "condition": "tool_pending == true" },
    { "from": "tools", "to": "chat" },
    { "from": "chat", "to": "__end__" }
  ]
}
```

`condition` and `skip_condition` use expr-lang expressions over board
variables. `${board.<name>}` references inside config strings resolve before
node decode, so `system_prompt` may interpolate upstream output. Node types
must be registered in the registry passed to `Build`; an unregistered type
fails `Build`, not `GraphDefinition.Validate`.

## Engine settings (`agent.Engine/graph`)

The deployment-facing factory is `core/graph/resource`. Its settings subtree:

| Key                              | Meaning                                                                        |
| -------------------------------- | ------------------------------------------------------------------------------ |
| `graph`                          | graph definition: literal content, `{file: ...}`, or `{embed: ...}` (required) |
| `script_runtime_name`            | name of the `agent.ScriptRuntime` dep bound to script nodes; default `js`      |
| `build.max_iterations`           | cap on total node invocations per run                                          |
| `build.timeout`                  | wall-clock bound for one `Execute` call (Go duration string, e.g. `1h`)        |
| `build.run_end_publish_timeout`  | deadline for publishing run-end lifecycle events                               |
| `build.max_node_retries`         | retries per node before the run fails                                          |
| `build.parallel.enabled`         | enable parallel waves                                                          |
| `build.parallel.branch_timeout`  | per-branch wall-clock bound                                                    |
| `build.parallel.max_concurrency` | max concurrent branches                                                        |
| `build.parallel.max_branches`    | max total branches per wave                                                    |
| `build.parallel.merge_strategy`  | `first_write_wins` or `last_write_wins`                                        |

Engine deps are derived from the definition: an inference node needs the
`inference` dep (explicit `model`) and/or `router` dep (no `model`), a tool
node needs `tools`, a script node needs `script_runtime`. `workspace` and
`sandbox` are optional and unlock the script `fs` / `shell` globals (below).

## Node types

### `inference` — single-shot LLM generation

Runs one `Generate` call: the channel tail is the current turn's input, the
assistant message (tool calls included) is appended to the same channel.
The node never executes tool calls — a `tool_calls` finish reason is flagged
onto `tool_pending_key` and the graph routes onward.

| Config field                             | Meaning                                                                                                           |
| ---------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `model`                                  | explicit target `{id: {provider, name}, profile?}`; absent defers selection to the wired router                   |
| `messages_channel`                       | board channel holding the conversation; empty means the main channel                                              |
| `system_prompt`                          | prepended system message when the context does not already start with one (may be `{file: ...}` / `{embed: ...}`) |
| `output_key`                             | board var receiving the full assistant `Message`                                                                  |
| `usage_key`                              | board var receiving the call's `inference.Usage`                                                                  |
| `tool_pending_key`                       | board var receiving `finish_reason == tool_calls` (the condition edges branch on)                                 |
| `stream`                                 | open a `GenerateStream`; text/reasoning deltas stream incrementally, the board still gets one assembled message   |
| `tools`                                  | named catalog tools the model may call this turn                                                                  |
| `all_tools`                              | send the catalog's entire visible set; with `tools`, names are declared `RequiredByName` and must exist           |
| `tool_choice`                            | constrain when/which tools are called                                                                             |
| `temperature` / `top_p`                  | sampling controls                                                                                                 |
| `max_output_tokens`                      | output token cap                                                                                                  |
| `reasoning_enabled` / `reasoning_effort` | universal reasoning switch / depth                                                                                |
| `response_format`                        | `text`, `json_object`, or `json_schema` (name + schema); generated text is re-validated against the schema        |
| `extensions`                             | provider knobs in the `{provider, id, fields}` wire form, resolved via the assembly's decoders                    |

Behavior:

- The channel tail must have role `user` or `tool`; everything before it is
  the request context. An empty channel or wrong role is a validation error.
- `model` uses the wired `inference.Assembly`; no `model` requires the
  `inference.Router` (selector/fallback chain picks the target).
- With `tools`/`all_tools` configured the tools dep's catalog must be wired;
  unknown names fail the node.
- Usage is reported to the host on every call. In stream mode a mid-stream
  failure commits the buffered partial text to the board and reports the
  last usage snapshot before propagating the error.

### `tool` — batch tool execution

Reads the `tool_call` parts off the channel tail's assistant message,
executes the whole batch through the tool dispatcher (model-issued call ids
preserved, so the provider can pair results next turn), and appends the
results as one `role=tool` message — again a valid tail for `inference`.

| Config field       | Meaning                                                                        |
| ------------------ | ------------------------------------------------------------------------------ |
| `messages_channel` | channel whose tail holds pending tool calls; empty means the main channel      |
| `results_key`      | board var receiving the raw `[]message.Result` for downstream nodes/conditions |

Behavior:

- The channel tail must have role `assistant` and carry at least one
  `tool_call` part.
- Allow-listing and approval policy live in the dispatcher's middleware
  chain, not in the node.
- Each result is published as a stream delta; a publish failure is logged
  and does not fail the node (the calls already executed — retries would
  re-run side effects).

### `script` — embedded script with host bindings

Runs inline JS or Lua source on the bound `agent.ScriptRuntime`. Decoding is
strict: unknown top-level config fields are errors.

| Config field | Meaning                                                                                           |
| ------------ | ------------------------------------------------------------------------------------------------- |
| `runtime`    | name of the wired `agent.ScriptRuntime` (must equal the engine's `script_runtime_name`); required |
| `source`     | inline script source; required (may be `{file: ...}` / `{embed: ...}`)                            |
| `name`       | execution label for errors and runtime pooling; defaults to the node ID                           |
| `config`     | becomes the script's `config` global; values may carry `${board.*}` references                    |

Each invocation assembles a fresh script environment with the standard
bridges below, executes the source, and maps control signals to Go errors.
Subscriptions opened mid-script are bound to the invocation and never
outlive it.

## Script bridges

Scripts never touch the Go context directly; the node exposes named globals
through `core/agent/bindings` (plus three graph-layer bridges). Availability
depends on the engine's deps:

| Global      | Wired when                              | Provides                             |
| ----------- | --------------------------------------- | ------------------------------------ |
| `board`     | always                                  | read/write board vars and channels   |
| `expr`      | always                                  | evaluate expr-lang expressions       |
| `host`      | always                                  | publish/emit/interrupt/askUser/usage |
| `run`       | always                                  | read-only run identity               |
| `node`      | always                                  | current graph node identity          |
| `tools`     | `tools` dep                             | call tools / list catalog            |
| `inference` | `inference` and/or `router` dep         | LLM generation, routing, streaming   |
| `stream`    | always (needs a host with an event bus) | subscribe to node stream deltas      |
| `parallel`  | always (active during parallel waves)   | cancel sibling branches              |
| `fs`        | `workspace` dep                         | workspace file operations            |
| `shell`     | `sandbox` dep                           | sandboxed command execution          |
| `runtime`   | always                                  | nested sub-script execution          |

### `board`

Direct read/write of the engine board.

| Method                           | Signature                                                         |
| -------------------------------- | ----------------------------------------------------------------- |
| `board.getVar(key)`              | `any`                                                             |
| `board.setVar(key, value)`       | —                                                                 |
| `board.deleteVar(key)`           | —                                                                 |
| `board.getVars()`                | `map`                                                             |
| `board.hasVar(key)`              | `bool`                                                            |
| `board.channel(name)`            | `array` of message objects (never null)                           |
| `board.setChannel(name, msgs)`   | throws on validation errors                                       |
| `board.appendChannel(name, msg)` | throws on validation errors                                       |
| `board.MAIN_CHANNEL`             | the reserved default channel name (use it instead of the literal) |

Messages use the inference wire format: `{role, content: {parts: [...]}}`
with parts like `{"type": "text", "text": "..."}` or
`{"type": "image", "source": {...}}`. Decoding is strict, so typos surface
as errors.

```js
board.setVar("plan", "1. read files\n2. summarize");
board.appendChannel(board.MAIN_CHANNEL, {
  role: "user",
  content: { parts: [{ type: "text", text: "go" }] },
});
```

### `expr`

Evaluates expr-lang against an environment map (compiled programs are LRU
cached).

```js
var ok = expr.eval("score > 0.5 && !done", { score: 0.8, done: false });
```

### `host`

The script-side handle onto `agent.Host`. Every method returns nil / `""` on
a no-op host, so scripts can call them unconditionally.

| Method                                            | Signature                                             |
| ------------------------------------------------- | ----------------------------------------------------- |
| `host.publish(subject, payload)`                  | `error` — low-level escape hatch to any event subject |
| `host.emit(type, payload)`                        | void — per-node stream delta                          |
| `host.checkInterrupt()`                           | `{cause, detail} \| null`                             |
| `host.askUser({parts, schema, source, metadata})` | `{parts, metadata}`                                   |
| `host.reportUsage({input, output, total})`        | `error`                                               |

`host.emit` event types recognized by the graph script node:

| Type          | Payload                                      | Resulting delta                |
| ------------- | -------------------------------------------- | ------------------------------ |
| `token`       | string (or any value, JSON-stringified)      | text part delta                |
| `tool_call`   | JSON string or `{id, name, arguments}`       | tool call part delta           |
| `tool_result` | `{tool_call_id, content, is_error}`          | tool result part delta         |
| `part`        | canonical part wire object (`{"type": ...}`) | arbitrary `message.Part` delta |
| anything else | —                                            | passthrough delta of that type |

Emission is fire-and-forget: publish failures are dropped, not thrown.

### `run`

Read-only run identity, sourced from the ambient `RunInfo`. All getters
return `""` when unset, so scripts can branch on absence directly.

| Method                    | Returns                                  |
| ------------------------- | ---------------------------------------- |
| `run.get_run_id()`        | run id                                   |
| `run.get_task_id()`       | task id                                  |
| `run.get_agent_id()`      | agent id                                 |
| `run.get_context_id()`    | conversation id                          |
| `run.get_parent_run_id()` | parent run id (empty for top-level runs) |

### `node`

Per-step identity of the executing graph node (distinct from `run`, which is
immutable across a whole run).

| Method        | Returns                         |
| ------------- | ------------------------------- |
| `node.id()`   | the node's ID in the definition |
| `node.type()` | the registered node type name   |

### `tools`

Script-callable facade over the tool dispatcher/catalog pair, with an
allow-list set by the host (`WithAllowedToolNames` / `WithToolAllowAll`). By
default **no** tool is callable.

| Method                                         | Signature                                                      |
| ---------------------------------------------- | -------------------------------------------------------------- |
| `tools.call(name, argumentsJSON)`              | `{content, is_error, tool_call_id}`                            |
| `tools.callAll([{name, arguments, id?}, ...])` | same shape per entry, plus `name`; batch via the dispatcher    |
| `tools.list()`                                 | `[names]` the script is allowed to call                        |
| `tools.definitions()`                          | wire-ready tool declarations to splice into a generate request |

Denied entries get an `is_error` result in place; the rest of the batch
still runs. A model-issued `id` passed to `callAll` is forwarded verbatim,
which is required when results feed back into an LLM turn.

### `inference`

LLM generation from scripts. Requests/responses are the canonical
`GenerateRequest` / `GenerateResponse` wire JSON; the bridge performs
exactly one call per invocation, so multi-turn tool loops live in
script-land (check `finish_reason`, call the message's tool parts via
`tools.callAll`, continue with `input.role = "tool"`).

| Method                                                                       | Behavior                                                                |
| ---------------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| `inference.generate(request)`                                                | exact model (`model` key required) via the assembly                     |
| `inference.route(request)`                                                   | router-selected target (no `model` key); response gains `trace`         |
| `inference.explain(request)`                                                 | preflight against one exact model, no provider I/O                      |
| `inference.routeExplain(request)`                                            | preflight through the router; returns `{explanation, decision, limits}` |
| `inference.models()`                                                         | full catalog of model descriptors                                       |
| `inference.inspect(model)`                                                   | one model's descriptor                                                  |
| `inference.embed(request)` / `inference.routeEmbed(request)`                 | embedding twins of generate/route                                       |
| `inference.explainEmbed(request)` / `inference.routeExplainEmbed(request)`   | embedding twins of explain/routeExplain                                 |
| `inference.explainStream(request)` / `inference.routeExplainStream(request)` | local stream preflight without opening a provider stream                |
| `inference.stream(request)` / `inference.routeStream(request)`               | streaming twins returning `{next, result, close}`                       |

Stream handle: `next()` returns one event or `null` at EOF, `result()`
returns the accumulated `GenerateResponse` after `next()` returned `null`,
and `close()` abandons a stream early (idempotent).

Extensions ride a bridge-level `extensions` array of
`{provider, id, fields}`, resolved through decoders the host registered
with `WithExtensionDecoder`; unregistered identities fail.

```js
var resp = inference.route({
  context: board.channel(board.MAIN_CHANNEL).slice(0, -1),
  input: {
    role: "user",
    content: {
      content: "summarize the workspace",
      intent: { text: { tools: tools.definitions() } },
    },
  },
});
if (resp.finish_reason === "tool_calls") {
  // pull tool_call parts off resp.message, call tools.callAll, loop
}
```

### `stream`

Node stream subscriptions over the run's event bus. Subscriptions are
scoped to the script invocation and closed automatically when it ends or
when every subscribed node has terminated.

| Method                                                                | Signature                                                             |
| --------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `stream.subscribe_node({node_id \| node_ids, run_id?, buffer_size?})` | `{next, next_timeout_ms, current, close}`                             |
| `iter.next()`                                                         | `bool` — blocks for the next matching event                           |
| `iter.next_timeout_ms(ms)`                                            | `(bool, error)` — bounded wait; timeout returns false without closing |
| `iter.current()`                                                      | latest event map, or null                                             |
| `iter.close()`                                                        | idempotent early close                                                |

Options: `node_id` and `node_ids` are mutually exclusive; `run_id` must
match the current run (overrides are rejected); `buffer_size` is 1..4096,
default 256, with `DropOldest` backpressure so slow scripts still see
terminal lifecycle events.

Each event is a map with `event`, `envelope_id`, `id`, `subject`, `time`,
`run_id`, `node_id`, `agent_id`, plus payload fields. Lifecycle events are
`step.started` / `step.ended` (status `success` or `error`) /
`step.skipped`; stream deltas arrive as `event: "stream.delta"` with
`type` / `part` / `speculative` / `branch_id` fields.

```js
var iter = stream.subscribe_node({ node_id: "planner" });
while (iter.next_timeout_ms(5000)) {
  var ev = iter.current();
  if (ev.event === "step.ended") break;
}
iter.close();
```

### `parallel`

Parallel-fork controls. The controller exists only while a parallel wave is
in flight; outside one `cancelNode` returns `false` (fail open).

| Method                                | Signature                               |
| ------------------------------------- | --------------------------------------- |
| `parallel.cancelNode(nodeID, reason)` | `bool` — true if a branch was cancelled |

### `fs`

Workspace file operations (wired when the engine has a `workspace` dep; the
workspace's own scoping/deny rules apply).

| Method                    | Signature         |
| ------------------------- | ----------------- |
| `fs.read(path)`           | `(string, error)` |
| `fs.write(path, content)` | `error`           |
| `fs.exists(path)`         | `bool`            |
| `fs.delete(path)`         | `error`           |

### `shell`

Sandboxed command execution (wired when the engine has a `sandbox` dep).
Returns a result map instead of throwing on non-zero exits.

| Method                     | Signature                     |
| -------------------------- | ----------------------------- |
| `shell.exec(cmd, args...)` | `{exit_code, stdout, stderr}` |

The host may restrict commands with `WithAllowedCommands`; a rejected
command returns `exit_code: -1` with an error in `stderr`.

### `runtime`

Nested script execution. Child scripts inherit the parent's final bindings;
the child's `config` global is the second argument.

| Method                               | Signature                                             |
| ------------------------------------ | ----------------------------------------------------- |
| `runtime.execScript(source, config)` | `(signal, error)` — error signals throw in the parent |

Nested execution is a runtime capability, not a guarantee: a pool with one
busy VM degrades to a `not_available` signal instead of panicking.

## Example

```json
{
  "name": "assistant",
  "entry": "chat",
  "nodes": [
    {
      "id": "chat",
      "type": "inference",
      "config": {
        "messages_channel": "main",
        "tool_pending_key": "tool_pending",
        "tools": ["read_file", "list_dir", "grep"]
      }
    },
    {
      "id": "tools",
      "type": "tool",
      "config": {
        "messages_channel": "main",
        "results_key": "tool_results"
      }
    },
    {
      "id": "finalize",
      "type": "script",
      "config": {
        "runtime": "js",
        "source": { "file": "./nodes/finalize.js" }
      }
    }
  ],
  "edges": [
    { "from": "chat", "to": "tools", "condition": "tool_pending == true" },
    { "from": "tools", "to": "chat" },
    { "from": "chat", "to": "finalize", "condition": "tool_pending == false" },
    { "from": "finalize", "to": "__end__" }
  ]
}
```

```js
// nodes/finalize.js
var last = board.channel(board.MAIN_CHANNEL).at(-1);
board.setVar("summary", last.content.parts[0].text);
host.emit("token", "done: " + run.get_run_id());
```

## Sources of truth

`core/graph/definition.go`, `core/graph/graph.go`, `core/graph/execute.go`,
`core/graph/nodes/inference.go`, `core/graph/nodes/tool.go`,
`core/graph/nodes/script/`, `core/graph/resource/resource.go`,
`core/agent/bindings/`.
