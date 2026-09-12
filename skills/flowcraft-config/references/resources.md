# Resource sub-documents

Sub-documents are attached through `settings: {file: ...}` or inline content.
Each factory owns its settings schema. Standalone `--type` validation is
syntax-only (strict YAML conversion, single document); settings semantics
are checked when the host build decodes them through the factory.

## inference provider

```yaml
id: deepseek
spec:
  api: responses        # "responses" (default) or "chat"
  endpoint:             # transport only: where the API is, how it authenticates
    base_url: https://api.deepseek.com   # optional; default https://api.openai.com/v1
    routing: azure_deployment            # optional: deployment-path rewriting
    query: {api-version: "2025-04-01-preview"}   # optional: added to every request
    headers: {x-gw: "1"}                 # optional static headers (credentials go in auth)
    timeout: 90s                         # optional: bounds one wire attempt
  auth:                 # how the profile's api_key rides the wire
    scheme: header      # "bearer" (default), "header", or "none" (no credential)
    header: Api-Key
  wire:                 # dialect this endpoint speaks
    store: false        # default false: responses are not retained server-side
    reasoning_channel: summary   # "summary" (default) or "text"
    include_reasoning_payload: true
    reasoning_summary: detailed  # optional "auto"/"concise"/"detailed"; opt in to readable traces
    truncation: auto    # optional "auto"/"disabled": context-overflow policy (responses)
  catalog: declared     # "builtin_declared" (default) or "declared"
  request_metadata:     # optional; supported by the openai wire family
    envelope: request_fields   # any non-empty top-level body field; empty disables
  models:               # optional: declare/override catalog models
    - name: deepseek-flash
      kind: generate
      capabilities:     # optional: capability leaves (see "Model declarations" below)
        inputs: [text, image, data, tool_call, tool_result]
        outputs: [text]
        reasoning:
          kind: toggle     # "always" or "toggle"; toggle promises reasoning_enabled=false compiles on this surface
          effort_map:      # optional: canonical effort -> model wire level
            minimal: low
            low: low
            medium: high
            high: high
            xhigh: max
        # hosted_web_search: false — DeepSeek ignores server-side tools other
        # than function calls, so claiming hosted search would be a lie.
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
escaped as `\${...}`. Provider drivers are registered by the host
application from provider driver modules (outside `core/`).

## Model declarations

`spec.models` entries extend a provider's built-in catalog or override an
entry by name. An entry that names a built-in model under the same kind is
a **leaf-level patch**: capability leaves the entry writes replace only
those leaves, and everything unstated — other capability leaves, numeric
limits, and driver control facts such as Bytedance's `max_resolution` —
is inherited from the built-in entry. Unknown names start from the
conservative zero base, so every published capability must be stated.
Compatible endpoints configured through the OpenAI and Anthropic drivers
follow the same leaf semantics.

Reasoning kind `toggle` is a promise that `reasoning_enabled=false`
compiles on that provider surface; models whose wire cannot turn reasoning
off publish `always` instead. OpenAI-family reasoning off is
`reasoning.effort: "none"` and needs no per-model knob (`effort_none` was
removed); OpenAI `api: chat` catalogs always publish `always` because the
chat surface has no off route.

Embed models that accept custom output dimensions declare the capability
leaf `custom_embed_dimensions` (OpenAI, Azure, Bytedance). The old
top-level `dimensions:` key is gone, drivers without an embed family reject
the leaf, and accepted sizes come from the built-in catalog whitelist — a
declaration can restate the leaf on a whitelisted entry but cannot grant it
to a model without one.

Routing prefers targets whose declared outputs cover the request intent
and skips declared-incompatible tiers.

`request_metadata.envelope` names the top-level body field that receives
canonical `GenerateRequest.RequestMetadata`. It is supported by the
OpenAI driver, which covers OpenAI, Azure, DeepSeek, Kimi and compatible
gateways; the Anthropic, MiniMax, and Bytedance drivers keep their native
transports and report request metadata as `dropped` in the compile report.

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
          - model: {id: {provider: deepseek, name: deepseek-flash}}
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
      budget:               # per-round visible set
        max_definitions: 32
        max_bytes: 16384
      discovery:            # persistent discovery pool
        max_tools: 32
        max_bytes: 16384    # independent byte cap
        idle_rounds: 10
```

`tool_search` must stay `always`; any other exposure is rejected at host
build. It takes a `query` and an optional `limit` (default 8): matching
Direct/Deferred tools are loaded and added to the session discovery pool
automatically — there is no `select` step (a legacy caller that still
passes `select` is ignored) — and their real schemas become visible from
the next round. Executed calls refresh pool recency; entries idle beyond
`discovery.idle_rounds` (default 10) are evicted, and the pool never
exceeds `discovery.max_tools` / `discovery.max_bytes` (defaults 32 /
16 KiB), an independent byte cap. The per-round `budget` still caps what
the model actually receives each turn; raising `discovery.max_bytes`
above it keeps extra entries as a no-token loaded cache until they are
used or re-searched. The legacy `selected_retention` and `recent_window`
policy keys only seed `discovery.idle_rounds` when it is unset.

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
      telemetry: {enabled: true}
      result_limit: {max: 20000}
      timeout: {default: 30s}
      concurrency: {limit: 8}
    dynamic: {default: deferred, exposures: {tool_search: always}}
```

The `dynamic` subtree takes the same keys as the memory sample above
(`budget`, `discovery`, ...). Each middleware entry is optional; absent
entries are skipped. `recover`
converts tool panics into error results, `telemetry.enabled` records an
OpenTelemetry span plus executions/duration/error metrics and a warning
log per call, `timeout.default` bounds each call (calls that already
carry a deadline pass through), and `concurrency.limit` caps in-flight
executions. `result_limit.max` caps the runes of one result's text parts
and `result_limit.part_budget_bytes` caps the encoded size of its non-text
parts (images, audio, video, file references, structured data); an absent
budget means 1 MiB and `0` lifts the cap. Parts over budget are dropped and
the truncation marker (`result_limit.marker`, default `…[result truncated]`)
is appended.

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
