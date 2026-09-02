# Resource sub-documents

Sub-documents are attached through `settings: {file: ...}` or inline content.
Each factory owns its settings schema. Standalone `--type` validation is
syntax-only (strict YAML conversion, single document); settings semantics
are checked when the host build decodes them through the factory.

## inference provider

```yaml
id: deepseek
spec:
  api: chat             # "chat" (default) or "responses"
  base_url: https://api.deepseek.com   # optional override
  request_metadata:     # optional; supported by deepseek/openai/azure
    envelope: request_fields   # any non-empty top-level body field; empty disables
  models:               # optional: declare/override catalog models
    - name: deepseek-v4-flash
      kind: generate
      capabilities:     # optional: content kinds, hosted web search, reasoning control
        inputs: [text, data, tool_call, tool_result]
        outputs: [text]
        reasoning:
          kind: toggle     # "always" or "toggle"; legacy string "toggle" is still accepted
          effort_map:      # optional: canonical effort -> model wire level
            minimal: low
            low: low
            medium: high
            high: high
            xhigh: max
        hosted_web_search: true
      responses: true
profiles:
  - secrets:
      api_key: ${env:DEEPSEEK_API_KEY}
```

Provider IDs and profile IDs must be identifiers. Secret values use the
unified settings reference syntax (`${env:NAME}` for an environment
variable, `${base:rel}` / `~` / `${home:rel}` for paths, `${secret:NAME}`
or `${secret:store.NAME}` for declared `secret.Store` backends); a missing
variable fails the build. The deploy builder expands every settings
subtree with env/home/base enabled by default — literal `${` must be
escaped as `\${...}`. Model capabilities mirror the built-in catalog shape and are
validated by the driver; routing prefers targets whose declared outputs
cover the request intent and skips declared-incompatible tiers. Provider
drivers are registered by the host application from provider driver
modules (outside `core/`).

`request_metadata.envelope` names the top-level body field that receives
canonical `GenerateRequest.RequestMetadata`. It is supported by the
DeepSeek, OpenAI, and Azure drivers; other drivers (Anthropic, MiniMax,
Bytedance, Kimi, Qwen) keep their native transports and report request
metadata as `dropped` in the compile report.

## inference assembly

The assembly consumes provider resources through `deps`:

```yaml
infer:
  kind: inference.Assembly
  impl: unified
  deps:
    provider: provider
```

Routing is an optional `inference.Router` resource; see below.

## inference router

The router consumes one assembly as its `target` dep and reads the route
policy from its own `settings`:

```yaml
router:
  kind: inference.Router
  impl: unified
  deps:
    target: infer
  settings:
    generate:
      - tier: fast
        targets:
          - model: {id: {provider: deepseek, name: deepseek-v4-flash}}
            score: {quality: 0.8, speed: 0.9}
    retry:
      generate:
        max_attempts: 2
        max_total_attempts: 5
        backoff: {kind: exponential, initial: 100ms, max: 2s, multiplier: 2, jitter: full}
        retryable: [rate_limit, timeout, unavailable]
        fallback_on_retry_exhausted: true
    circuit_breaker:
      failure_threshold: 5      # consecutive transient failures; default 5
      recovery_window: 30s      # default 30s
      half_open_max_probes: 1   # default 1
```

`generate` / `embed` / `transcription` each list `tier` pools of exact
`model` targets plus optional `score` signals (`quality` / `economy` /
`speed` / `reliability`, all in `[0, 1]`); scores guide selection only.
Selection skips targets whose declared output capabilities cannot serve
the request intent; undeclared capabilities are treated as undeclared, not
unsupported. `retry` (per-operation, requires pools) and `circuit_breaker`
configure resilience. Build-time validation checks every target exists,
is not retired, and exposes the operation.

## workspace

```yaml
root: ./workspace
scoped:
  enabled: true
  deny_read: ["**/.env"]
  allow_write: ["**"]
  mandatory_deny: [".git/**"]
```

Relative roots resolve against the deployment loader's base directory.

## sandbox

```yaml
box:
  kind: sandbox.Runner
  impl: local
  settings:
    root: ./sandbox
```

The local runner is no-isolation and takes only `root`. The isolation
backends (`bwrap`, `seatbelt`) share a larger settings surface:

```yaml
box:
  kind: sandbox.Runner
  impl: bwrap            # or seatbelt
  settings:
    root: ./sandbox
    binary: /usr/bin/bwrap    # optional; resolved against the root
    writable_paths: [./out]   # optional; paths the sandbox may write
    readonly_root: true       # optional; keep the runner root read-only
    extra_flags: [--die-with-parent]  # bwrap only; policy-downgrading flags are rejected
```

`readonly_root` keeps the runner root read-only for every exec; explicit
`writable_paths` stay writable. The per-call counterpart is
`ExecOptions.Write` (`WriteWorkspace` zero value / `WriteReadOnly`) —
host code narrows a single call to read-only without changing settings;
`WriteReadOnly` on `sandbox/local` is rejected as unavailable.

`writable_paths` entries that resolve to the runner root conflict with
`readonly_root: true`: the host build rejects the combination instead of
silently dropping the entry (without `readonly_root` such an entry is
redundant and ignored).

## tool source / assembly

```yaml
sim:
  kind: tool.Source
  impl: sim
tools:
  kind: tool.Assembly
  impl: memory
  deps:
    tool: sim
  settings:
    dynamic:
      default: deferred
      exposures:
        tool_search: always
```

The `middleware` impl is the same assembly with a settings-declared
middleware chain; the `memory` impl rejects the `middlewares` key:

```yaml
tools:
  kind: tool.Assembly
  impl: middleware
  deps:
    tool: sim
  settings:
    middlewares:
      recover: {enabled: true}
      timeout: {default: 30s}
      concurrency: {limit: 8}
    dynamic: {default: deferred, exposures: {tool_search: always}}
```

Each middleware entry is optional; absent entries are skipped. `recover`
converts tool panics into error results, `timeout.default` bounds each
call (calls that already carry a deadline pass through), and
`concurrency.limit` caps in-flight executions.

MCP servers attach as a `tool.Source/mcp` resource; attach is best-effort
with background reconnection, and `required: true` marks a server the host
should `WaitReady` on:

```yaml
sim:
  kind: tool.Source
  impl: mcp
  settings:
    servers:
      - name: filesystem
        transport: stdio           # stdio | http
        command: npx
        args: ["-y", "@modelcontextprotocol/server-filesystem"]
        env: {TOKEN: ${env:MCP_TOKEN}}
        prefix: fs                  # tool namespace; default "<name>__"
        resources: true             # bridge list_resources / read_resource tools
        required: true
      - name: remote
        transport: http
        url: https://mcp.example.com/mcp
        headers: {Authorization: "Bearer ${env:MCP_TOKEN}"}
        http_timeout: 30s
```

## memory

Memory implementations are app-registered. `core/memory` supplies contracts
and hooks; each implementation owns its settings document. The core hooks
bind the whole assembly as their `memory` dep:

```yaml
agents:
  assistant:
    prepare:
      - type: memory.context       # hook.prepare seed hook
        deps:
          memory: memories
        settings:
          query: {literal: "relevant prior conversation"}  # or board / current_message / recent_only
          scope: {runtime_id: memories, user_id: user-1, agent_id: assistant}
          conversation_id: conv-1  # optional; defaults to the request ContextID
          dataset_ids: [docs]      # optional
          budget: {max_tokens: 2000, max_items: 50, max_chars: 8000}
          min_score: 0.5
          output: memory_items     # required; non-reserved board var
          render: {output: memory_text, gotmpl: {max_chars: 8000}}
    commit:
      - type: memory.turn          # hook.commit durable finalizer
        deps:
          memory: memories
        settings:
          scope: {runtime_id: memories, user_id: user-1, agent_id: assistant}
          conversation_id: conv-1  # optional; defaults to the request ContextID
          channel: __main_channel  # optional; defaults to the main channel
```

`memory.context` requires exactly one `query` source and a non-reserved
`output` var; recall is hard-partitioned by scope. `memory.turn` commits
the turn's channel idempotently per run id.

## event bus

```yaml
events:
  kind: event.Bus
  impl: memory
  settings:
    route_cache_size: 1024  # optional: positive caps the route cache, zero disables it
```

## script runtime

```yaml
js:
  kind: agent.ScriptRuntime
  impl: js
  settings:
    pool_size: 4              # positive; number of pooled VMs
    max_call_stack_size: 512  # js only; positive call-stack bound
    max_exec_time: 30s        # Go duration; zero disables the cap

lua:
  kind: agent.ScriptRuntime
  impl: lua
  settings:
    pool_size: 4
    max_exec_time: 30s
```

Script runtimes are wired into a graph engine as the `script_runtime` dep
(see [graph.md](graph.md)).

## delegation

```yaml
dir:
  kind: delegation.Directory
  impl: local   # no settings; binds the deployment's agents at wire time

prov:
  kind: delegation.SessionProvider
  impl: random  # no settings; fresh ContextID per delegation, never persists

svc:
  kind: delegation.Service
  impl: local
  deps:
    directory: dir
    # backend: async            # optional delegation.AsyncBackend; absent = sync-only
    # session_provider: prov    # optional identity policy
  settings:
    max_concurrency: 4          # positive; default 4
    max_depth: 8                # positive; default 8
    timeout: 5m                 # Go duration; zero leaves the caller's context
    idempotency_retention: 24h  # positive; how long responses stay replayable
    defer_workers: true         # start async workers on Start instead of at build

dtools:
  kind: tool.Source
  impl: delegation
  deps:
    directory: dir  # no settings; exposes delegate / delegation_status / delegation_targets
```

Expose the service on every turn host with
`runtime.Builder.WithResultHostFactory` plus `delegation/hostwrap`; the
directory binds the current deployment generation, so reloads delegate
against the new generation's agents.

### Async streaming

Async delegations stream subagent deltas to the caller's live sinks by
default: the submit side stores the caller's sinks in a service-side
escrow and the worker restores them, so no YAML settings are needed.
Streams carry lineage headers (`run_id`, `parent_run_id`, `tool_call_id`,
`agent_id`).

Cross-process delivery (worker without the escrow: TTL expiry, restart,
separate process) is wired in Go, not YAML — build a
`runtime.StreamExportRegistry`, register its resolver/exporter on the
delegation service factory, and expose the service on turn hosts:

```go
reg := runtime.NewStreamExportRegistry(map[string]event.Bus{"events": bus})
delegation.NewServiceFactory(
    delegation.WithStreamTargetResolver(reg.Resolver),
    delegation.WithStreamTargetExporter(reg.Exporter),
)
// + runtime.Builder.WithResultHostFactory(hostwrap.Wrap)
```

Target reachability: `conversation` targets resolve to a live sink
registered in the resolving process's registry (same-process recovery
only); `bus` targets forward onto a named event bus and are the
cross-process option. `StreamRef.Target` is single-valued — the
in-process escrow keeps every sink, the cross-process path restores
exactly one, and the exporter prefers bus targets. Sinks describe
themselves via `delegation.StreamTargetProvider`, so UI decorators can
pass the description through. See
[docs/guides/delegation.md](../../../docs/guides/delegation.md) for the
full lifecycle.

## checkpoint store

```yaml
cps:
  kind: checkpoint.Store
  impl: workspace
  deps:
    workspace: ws
  settings:
    prefix: agent/checkpoints  # optional; default "agent/checkpoints"
```

`runtime.checkpoint_store` names a resource implementing the
`agent.CheckpointStore` contract; `sessions.resume` requires one (see
[runtime.md](runtime.md)). Alternative backends are app-registered outside
`core/` and own their settings schema.

## graph

Graph definitions are engine settings:

```yaml
engine:
  kind: agent.Engine
  impl: graph
  settings:
    graph: {file: ./graphs/assistant.yaml}
    script_runtime_name: js
```

See [graph.md](graph.md) for the JSON schema.
