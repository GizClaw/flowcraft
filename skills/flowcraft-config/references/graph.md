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
      "messages_channel": "__main_channel"
    }},
    {"id": "tools", "type": "tool", "config": {
      "messages_channel": "__main_channel",
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
| `model` | explicit target `{id: {provider, name}, profile?}`; omit in favor of the wired router — pin only for tests or single-target demos |
| `model_hint` | per-call router preference: `provider/name` or a bare name (e.g. `${board:model}`); ignored when `model` is set |
| `messages_channel` | board channel holding the conversation; empty means the main channel (`__main_channel`) |
| `system_prompt` | prepended system message when the context does not start with one (may be `{file: ...}` / `{embed: ...}`) |
| `output_key` / `usage_key` / `tool_pending_key` | board vars receiving the message / usage / tool-pending flag |
| `undefined_tool_recovery` | `{enabled, max_per_run}`: store undefined-tool rejections as board feedback (never in the transcript); disabled by default |
| `recover_pending_key` / `recover_count_key` / `recover_feedback_key` | board vars receiving the recovery marker / per-run counter / feedback text (feedback defaults to `__recover_feedback.<node id>`) |
| `stream` | stream deltas incrementally; the board still gets one assembled message |
| `tools` / `all_tools` | named catalog tools, or the catalog's entire visible set |
| `tool_choice` | constrain when/which tools are called |
| `intent` | canonical execution envelope `{text, image, audio, video}` with per-modality controls (see below) |
| `extensions` | provider knobs `{provider, id, fields}` |
| `request_metadata` | opaque `map[string]string` copied onto the canonical generate request; keys are deployment-defined |

**Model selection: prefer the router.** Inference nodes should omit
`model` and rely on the wired `inference.Router`: the router's policy
(tiers of `{model, score}` targets plus retry / circuit-breaker) selects
per request, filters targets by their declared capabilities, and falls back
across tiers. Hardcoding `model` pins the node to one provider/model and
bypasses routing — keep it only for tests or single-target deployments.
Use `model_hint` (e.g. `${board:model}` from user input) for per-call
preferences against the router.

`intent` is authoritative and covers every generation modality: text
controls (`response`, `max_output_tokens`, `tools`, `tool_choice`,
`temperature`, `top_p`, `reasoning_enabled`, `reasoning_effort`
(`minimal|low|medium|high|xhigh`), image
(`size`, `aspect_ratio`, `count`, `seed`, `output_format`, `delivery`),
audio/tts (`voice`, `format`, `speed`, `count`), and video
(`duration_millis`, `resolution`, `aspect_ratio`, `seed`, `watermark`).
When `intent` is absent the node defaults to plain text generation.
`tools` / `all_tools` / `tool_choice` remain node-level sugar: they resolve
the wired catalog into `intent.text.tools` / `intent.text.tool_choice` and
may not be combined with an intent that declares those fields itself.
Legacy node-level sampling/reasoning/response-format keys were removed —
strict decode rejects them; put them under `intent.text` instead.

`request_metadata` values are forwarded only when the selected driver's
deployment configures `request_metadata.envelope`. Drivers without a native
channel (or with forwarding disabled) report a `dropped` compile decision
for the metadata field, so no metadata is silently ignored.

`model_hint` is a request-level preference consumed only by the router: it
never bypasses routing and falls back to the default policy when the hint
is unknown, malformed, or ambiguous. A response naming tools absent from
the exposed definitions fails with a distinguishable `undefined_tool`
error; with `undefined_tool_recovery` enabled the node stores a user-role
feedback text under `recover_feedback_key` (defaulting to the reserved
per-node `__recover_feedback.<node id>` var), sets `recover_pending_key`
(clearing `tool_pending_key`), and returns success. The feedback never
enters the messages channel; the recovered round consumes the stored text
as its current input. The graph must route the recovered round back to
inference — never to a tool node, which would execute the rejected call.
`max_per_run` defaults to 2; past it the rejection fails the node.

### `tool` — batch tool execution

Reads the `tool_call` parts off the channel tail's assistant message,
executes the whole batch through the tool dispatcher (call ids preserved),
and appends the results as one `role=tool` message.

| Config field | Meaning |
| --- | --- |
| `messages_channel` | channel whose tail holds pending tool calls; empty means the main channel (`__main_channel`) |
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

### Custom node types (`graph.NodeType`)

Beyond the three built-ins, node types are deployment resources. A resource
of kind `graph.NodeType` produces a value implementing
`graph.NodeTypeRegistrar`; the graph engine factory registers every
`node_type` dependency into the engine's registry before `Build`, so the
graph definition can reference the type by name. Custom node types
participate in the normal resource DAG (own deps, built once, shareable
across agents); the engine accepts deps under `node_type` or
`node_type.<suffix>` (the `Many` dep form, like `tool.*` sources).

The built-in script impl (`graph.NodeType/script`) defines the node
behaviour as an embedded script on the bound `agent.ScriptRuntime` with the
same bridges as the built-in `script` node; the node's config in the graph
definition becomes the script's `config` global.

| Settings field | Meaning |
| --- | --- |
| `type` | node type name graphs reference (must not collide with built-ins or other mounted types) |
| `source` | handler source: inline string or `{file: ...}` / `{embed: ...}` (required) |
| `desc` | optional human-readable type description |
| `reads` / `writes` | static I/O roles (`kind: var\|messages`, `name` or `config_key`, `required`); enforced at Build and invocation |

Deps of `graph.NodeType/script`: `script_runtime` (required), plus optional
`tools`, `inference`, `router`, `workspace`, `sandbox` enabling the same
globals the built-in script node unlocks.

```yaml
resources:
  greet:
    kind: graph.NodeType
    impl: script
    deps:
      script_runtime: js
    settings:
      type: greet
      source: board.setVar("greeting", "hi " + config.name)
      writes:
        - kind: var
          name: greeting
          required: true
```

Wire the type into the engine with a `node_type` dep, then reference it by
name in the graph:

```yaml
agents:
  asst:
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        node_type.greet: greet
```

```json
{
  "name": "greeter",
  "entry": "hello",
  "nodes": [
    {"id": "hello", "type": "greet", "config": {"name": "${board:user}"}}
  ],
  "edges": []
}
```

Go-backed custom node types follow the same contract: a host or plugin
registers a `graph.NodeType` resource factory whose value implements
`graph.NodeTypeRegistrar` (typically by calling `graph.RegisterType` with a
closure capturing the resource's deps).

### `board`

- `getVar(key)`, `setVar(key, value)`, `deleteVar(key)`, `getVars()`,
  `hasVar(key)`
- `resolve(str)` → `any`, `resolveString(str)` → `string` — typed / text
  `${board.*}` expansion (missing references error unless a default is given)
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
`finish` (`{finish_reason, request_id?, response_id?}` — typed finish
delta), `provider_outputs` (`[{provider, extension, value}]` —
provider_outputs delta), or a passthrough type. Emission is fire-and-forget;
payloads that do not decode into the type's required shape (e.g. a
`tool_call` without `id`, a `finish` without `finish_reason`, an empty
`provider_outputs`) are skipped instead of published as empty deltas.

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
- `transcribe(request)` / `routeTranscribe(request)` — whole-audio
  transcription; the route twin gains `trace`
- `explainTranscribe(request)` / `routeExplainTranscribe(request)` —
  transcription preflight; the route twin returns `{explanation, decision, limits}`
- `transcribeSession(request)` / `routeTranscribeSession(request)` — duplex
  session handle `{send, next, result, interrupt, finish, close}`; the route
  twin attaches `trace` to the result

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
`core/graph/resource/resource.go`, `core/graph/resource/node_type.go`,
`core/agent/bindings/`,
`docs/guides/graph.md`.
