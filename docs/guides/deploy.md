---
layout: default
title: Deployment Assembly
---
# Deployment Assembly Guide

`sdkx/deploy` is FlowCraft's **assembly layer**: one YAML document
names shared resources, agents and their lifecycle hooks, and optional
application-runtime configuration. One Go call assembles the deployment
and returns runnable agent instances.

It does **not** own turn loops, sessions, conversation persistence,
interrupts, or application startup. `sdkx/runtime` can own those
process-level concerns above the assembled `Instance` values; deploy
itself is never the session owner.

## What you get from one Build call

- Shared, named resources built in dependency order (workspaces,
  sandboxes, an inference runtime, a tool catalog, script runtimes,
  event bus, ...).
- Named agent instances bound to those resources through an
  engine-specific dep contract (graph is the built-in engine; custom
  engines plug in).
- Lifecycle hooks for board seeding, observation, and disposition
  decisions, all declarative.
- An `*deploy.Result` that owns the document-built lifecycle, plus
  typed accessors (`Instance`, `ResourceAs[T]`) for the application
  to borrow or hand off the rest.

The application still owns:

- The `agent.Host` (request loop, streaming, interrupts, host-side
  capabilities like `EventBus`, `MemoryStores`).
- Anything resolved through a `Source` rather than a `Resource`
  (process-wide clients, shared stores, callbacks).

## Concepts

### Document

`deploy.Document` is the parsed YAML. It has three top-level areas:

| Area        | What it names                              | Who owns its schema or construction              |
| ----------- | ------------------------------------------ | ------------------------------------------------ |
| `resources` | shared, long-lived objects                 | registered `ResourceFactory` per `(kind, impl)`  |
| `agents`    | named agent instances with engine + hooks  | engine factory + registered hook factories       |
| `runtime`   | optional application-runtime configuration | preserved opaquely by deploy; strictly decoded by `sdkx/runtime` |

```yaml
version: v1
resources:
  infer:
    {
      kind: inference.Assembly,
      impl: yaml,
      settings: { file: ./inference.yaml },
    }
agents:
  greeter:
    engine: { kind: graph, settings: { graph: ./graphs/greeter.json } }
    deps: { inference: infer }
```

The `runtime` subtree is deliberately opaque to `deploy.Parse`; the public
`sdkx/runtime.Builder` decodes it strictly. Runtime resource references are
passed to deployment assembly as external consumers, so those resources are
retained even when no agent binds them and they do not set `export: true`.
See the [Runtime and Sessions guide](runtime.md) for its complete schema,
registration, ownership, and turn APIs.

Everything else — every `kind`, `impl`, `engine.kind`, hook `type` —
is looked up in a registry. `sdkx/deploy` ships **exactly one** entry
of its own: the `discard_on_interrupt` referee. Every other kind must
be registered before `Build`, or parsing/building fails fast with
`<name> is not registered`.

### Resources vs sources

Both fill dependency slots, but they have different ownership:

|              | Resource                                          | Source                                                                        |
| ------------ | ------------------------------------------------- | ----------------------------------------------------------------------------- |
| Built by     | `Builder.Build` (topological order)               | Application code, called at Build time                                        |
| Closed by    | `Result.Close` (reverse order)                    | Application — `Result.Close` never touches it                                 |
| Addressed as | scalar `name` or `name/item`                      | mapping `{source: name, ref: id}`                                             |
| Use when     | The deployment should construct and own the value | Process-wide client, shared store, callback — the application already owns it |

```yaml
deps:
  inference: infer # whole resource
  workspace: ws/project # one item inside a container resource
  store: { source: host.memory, ref: default } # borrowed from the host
```

### Container vs whole resources

A resource is a **container** if it implements `deploy.ItemResolver`
(workspace registry, sandbox registry). It binds one item per dep:

```yaml
deps:
  workspace: ws/project
  sandbox: box/coding
```

A resource may expose both its whole assembly and a deliberately typed item.
For example, graph agents bind the whole `inference.Assembly` as `infer`, while
resources that need only exact inference calls bind its
`inference.Runtime` item as `infer/runtime`. Undeclared item names are build
errors.

### Agents and engines

An `agent.Engine` is anything that turns a `Board` plus a request
into a `Result`. The graph engine is the built-in; custom engines
register through `agent.NewFactory` on the same registry the
`Builder` was constructed with.

Each engine publishes its own `EngineSpec`: a list of named deps with
required types. The document's `agent.deps` is keyed by those names:

```yaml
agents:
  researcher:
    engine:
      kind: graph
      settings:
        graph: ./graphs/research.json
    deps:
      inference: infer # matched against the graph factory's DepSpec
      tools: kit
      workspace: ws/project
```

Unknown dep names, missing required deps, and kind/type mismatches
are all rejected at build time — a single source of validation
across the document.

An agent recipe can be inline or stored in one versioned YAML file.
In both forms, the root `agents` map key is the agent ID; an agent
file does not repeat the ID:

```yaml
# deployment.yaml
version: v1
agents:
  dispatcher: # Agent.ID is "dispatcher"
    card:
      name: Dispatcher
      description: Routes work to specialists
    tools: [delegate, delegation_status]
    engine:
      kind: graph
      settings:
        graph: ./graphs/dispatcher.json
    deps:
      tools: tools
    referees:
      - type: delegation_handoff

  researcher: # Agent.ID is "researcher"
    file: ./agents/researcher.agent.yaml
```

```yaml
# agents/researcher.agent.yaml -- exactly one agent recipe
version: v1
card:
  name: Researcher
  description: Produces sourced research
tools: [delegate, delegation_status]
engine:
  kind: graph
  settings:
    graph: ./graphs/researcher.json
deps:
  tools: tools
policy:
  max_revise: 2
  artifact_channels: [report]
```

`file` and inline fields are mutually exclusive. Agent files require
their own `version: v1` and reject unknown fields. Relative agent file
paths are resolved against `deploy.WithBaseDir(configDir)` on the
`Builder`; paths inside engine settings are interpreted by that engine,
so configure its base directory separately when required.

### Lifecycle hooks

Four hook sections, all sharing the same `{type, deps, settings}`
shape:

| Section    | Returns                                | Runs                                               | Use for                                               |
| ---------- | -------------------------------------- | -------------------------------------------------- | ----------------------------------------------------- |
| `prepare`  | `agent.Preparer` (mutates board)       | Before the engine, in declaration order            | Loading history, recall, system prompt, board seeding |
| `referees` | `agent.Referee` (returns a `Decision`) | After each attempt, merged by the harness           | Disposition, quality gates                            |
| `commit`   | `agent.Committer` (returns an error)   | Once, after the final accepted result               | Durable transcript persistence or outbox enqueueing   |
| `observe`  | `agent.Observer` (read-only)           | Lifecycle events (start, interrupt, revise, end)   | Logging, metrics, notifications, snapshots            |

`policy` is **not** a hook — it is a per-call struct
(`max_revise`, `artifact_channels`) read by the engine factory.

Only `discard_on_interrupt` is built in. Every other hook kind must
be registered on the `Builder` (`RegisterPreparer`, `RegisterReferee`,
`RegisterCommitter`, `RegisterObserver`).

## First agent

The smallest useful deployment: one resource, one agent, one execute
call.

`deployment.yaml`:

```yaml
version: v1
resources:
  infer:
    kind: inference.Assembly
    impl: yaml
    settings:
      file: ./inference.yaml

agents:
  greeter:
    card:
      name: Greeter
      description: Says hello
    engine:
      kind: graph
      settings:
        graph: ./graphs/greeter.json
    deps:
      inference: infer
```

`main.go`:

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/GizClaw/flowcraft/sdk/agent"
    "github.com/GizClaw/flowcraft/sdkx/deploy"
    graphagent "github.com/GizClaw/flowcraft/sdkx/agent/graph"
    "github.com/GizClaw/flowcraft/sdkx/inference/config/yaml"
)

func main() {
    ctx := context.Background()

    engines := agent.NewRegistry()
    engines.MustRegister(graphagent.NewFactory())

    builder := deploy.NewBuilder(engines)
    builder.MustRegisterResource(yaml.NewDeployFactory(nil, nil))

    data, _ := os.ReadFile("deployment.yaml")
    doc, err := deploy.Parse(data)
    if err != nil { panic(err) }

    result, err := builder.Build(ctx, doc)
    if err != nil { panic(err) }
    defer result.Close()

    inst, _ := result.Instance("greeter")
    out, err := inst.Execute(ctx, agent.Request{Message: "hello"})
    if err != nil { panic(err) }
    fmt.Println(out.Committed)
}
```

The pattern: **register the parts you use, then Build, then Execute**.
The document only names what it needs; the Go side declares what
exists.

## The resource area

### Shape

```yaml
resources:
  <name>:
    kind: <resource category> # matched against DepSpec.Type when bound whole
    impl: <registered implementation>
    export: false # set true to retrieve from Result
    deps: { <factory dep name>: <DepRef> }
    settings: <impl-owned, strictly decoded>
```

- `kind` + `impl` select a `ResourceFactory` registered on the
  `Builder`.
- `deps` matches the factory's `ResourceSpec.Deps` (names + required
  types).
- `settings` is opaque to the loader; the factory must call
  `deploy.DecodeSettings[T]` so unknown keys fail the build.
- `export: true` lets the application retrieve a value via
  `deploy.ResourceAs[T](result, "<name>")` even if nothing binds it.

Anything built and not consumed by an agent, a hook, another resource, or an
explicit external consumer is dead configuration and fails the build.

### First-party impls

| Kind                      | Impl            | Result                          | Lives in                        |
| ------------------------- | --------------- | ------------------------------- | ------------------------------- |
| `workspace.Registry`      | `yaml`          | workspace container             | `sdkx/workspace/config`         |
| `sandbox.Registry`        | `yaml`          | sandbox container               | `sdkx/sandbox/config`           |
| `inference.Assembly`      | `yaml`          | runtime (+ optional router)     | `sdkx/inference/config/yaml`    |
| `tool.Assembly`           | `yaml`          | catalog + executor              | `sdkx/tool/config`              |
| `agent.ScriptRuntime`     | `js`            | JavaScript runtime              | `sdkx/agent/jsrt`               |
| `agent.ScriptRuntime`     | `lua`           | Lua runtime                     | `sdkx/agent/luart`              |
| `event.Bus`               | `memory`        | in-process bus                  | `sdkx/event/config`             |
| `delegation.AsyncBackend` | `kanban-memory` | asynchronous delegation backend | `sdkx/delegation/kanban/config` |
| `memory.Assembly`         | `yaml`          | runtime container                | `sdkx/memory/config/yaml`       |

Script runtimes share a kind because graphs pick one per agent
(`engine.settings.script_runtime_name`), but JS and Lua register as
distinct `impl` values.

### Sub-documents: file vs inline

The `workspace`, `sandbox`, `inference`, and `tool` resources wrap
their modules' own versioned YAML. Their `settings` accept **exactly
one** source:

```yaml
# File form — keep large sections reviewable on their own.
settings:
  file: ./inference.yaml

# Inline form — keep the whole deployment in one file.
settings:
  inline:
    version: v1
    providers:
      - id: openai
        driver: openai
        profiles:
          - secrets:
              api_key: { resolver: env, key: OPENAI_API_KEY }
```

`file` and `inline` are mutually exclusive. A mistyped key that
silently fell back to an empty inline document would surface much
later as a missing ref — that's why exactly-one is enforced.
The nested document is parsed by the owning module and retains its
own version field and strictness rules; there is no second schema to
keep in sync.

### Custom resources

A `ResourceFactory` is one Go type. Register it once on the `Builder`,
and it plugs into the document as a `(kind, impl)` pair:

```go
type myFactory struct{ /* config, client, etc. */ }

func (f *myFactory) Spec() deploy.ResourceSpec {
    return deploy.ResourceSpec{
        Kind: "my.Kind", Impl: "default",
        Deps: []deploy.ResourceDepSpec{
            {Name: "inference", Type: "inference.Assembly", Required: true},
        },
    }
}

func (f *myFactory) New(ctx context.Context, in deploy.ResourceInput) (any, error) {
    settings, err := deploy.DecodeSettings[mySettings](in.Settings)
    if err != nil { return nil, err }
    infer, _ := in.Dep("inference")
    return buildMyResource(ctx, settings, infer)
}

builder.MustRegisterResource(&myFactory{ /* ... */ })
```

```yaml
resources:
  my_resource:
    kind: my.Kind
    impl: default
    deps: { inference: infer }
    settings: { ... }
```

The factory decides whether its result is a container
(`ItemResolver`) or a whole object. If it implements `io.Closer`,
`Result.Close` will close it in reverse construction order.

## The agent area

```yaml
agents:
  <id>:
    card: { name, description }
    tools: [<allow-list>] # promoted to Run.ToolAllowList
    engine: { kind, settings }
    deps: { <engine dep name>: <DepRef> }
    prepare: [{ type, deps, settings }]
    referees: [[same shape]]
    commit: [[same shape]]
    observe: [[same shape]]
    policy: { max_revise: N, artifact_channels: [...] }
```

- `card` is the declarative subset of `agent.AgentCard`.
- `tools` is a per-agent allow-list (policy gate, validated at build
  time against the tool catalog).
- `engine` is opaque to the loader; the registered engine factory
  decodes and validates `settings` strictly.
- `deps` is keyed by the engine's `EngineSpec`; type mismatches fail
  the build.
- `policy` is a per-call struct, not a hook. The graph engine reads
  `max_revise`; custom engines can read whichever fields they
  declared.

### Engines

The graph engine (`sdkx/agent/graph`) is the built-in. Its
settings accept a graph definition by file path, by explicit
`{file: ...}`, or by `{inline: ...}`:

```yaml
engine:
  kind: graph
  settings:
    graph:
      file: ./graphs/research.json
    script_runtime_name: js
    build:
      max_iterations: 100
      timeout: 5m
      run_end_publish_timeout: 5s
      max_node_retries: 2
      parallel:
        enabled: true
        max_concurrency: 8
        max_branches: 32
        merge_strategy: last_write_wins
```

File definitions are limited to 1 MiB. Unknown graph fields are
rejected. Built-in merge strategies: `first_write_wins`,
`last_write_wins`. `run_end_publish_timeout` bounds the best-effort terminal
event publication and must be greater than zero.

The graph factory's dep contract:

| Name             | Type                  | Required when                                    |
| ---------------- | --------------------- | ------------------------------------------------ |
| `inference`      | `inference.Assembly`  | the graph contains an inference node             |
| `tools`          | `tool.Assembly`       | the graph contains tool nodes or inference tools |
| `workspace`      | `workspace.Workspace` | scripts need filesystem access                   |
| `sandbox`        | `sandbox.Runner`      | scripts need command execution                   |
| `script_runtime` | `agent.ScriptRuntime` | the graph contains a script node                 |

Custom engines register through `agent.NewFactory` with their own
`EngineSpec`, then appear in the document as `engine.kind: <name>`.

## Lifecycle hooks in practice

A hook is a small Go factory. Register it once, then reference it by
`type` in the document:

```go
builder.RegisterPreparer("recall", func(ctx context.Context, in deploy.HookInput) (agent.Preparer, error) {
    settings, err := deploy.DecodeSettings[recallSettings](in.Settings)
    if err != nil { return nil, err }
    store, _ := in.Dep("store")
    return buildRecallPreparer(settings, store), nil
})
```

```yaml
agents:
  researcher:
    prepare:
      - type: recall
        deps:
          store:
            source: host.memory
            ref: default
        settings:
          max_hits: 8
    commit:
      - type: transcript
        deps:
          store:
            source: host.memory
            ref: default
        settings:
          channel: main
    referees:
      - type: discard_on_interrupt # built-in
        settings:
          reason: barge-in
          causes: [user_input, user_cancel]
```

Hooks can bind the same kinds of dep references as resources — they
read the resource area through the same `DepRef` resolution path,
which is how a recall preparer reaches a memory store the deployment
built.

A `memory.Assembly` exposes its runtime item as `memory/runtime`.
Memory lifecycle hooks therefore bind it directly:

```yaml
prepare:
  - type: memory.load
    deps: {runtime: memory/runtime}
    settings: {into: transcript, limit: 50}
  - type: memory.recall
    deps: {runtime: memory/runtime}
    settings:
      into: hits
      query: {board: query}
      top_k: 8
commit:
  - type: memory.append
    deps: {runtime: memory/runtime}
    settings: {channel: __main_channel}
```

`memory.recall.settings.query` accepts exactly one of `literal` or `board`.
`board` reads the Board passed by the previous preparer. The default seeder
copies `Request.Inputs` into Board vars first, so it supports both request
inputs and values produced by earlier preparers without template expansion.

An embedding-enabled memory resource depends on the runtime item exported by
the inference assembly:

```yaml
# deploy.yaml
version: v1
resources:
  infer:
    kind: inference.Assembly
    impl: yaml
    settings: {file: ./inference.yaml}
  memory:
    kind: memory.Assembly
    impl: yaml
    deps:
      inference: infer/runtime
    settings: {file: ./memory.yaml}
agents:
  researcher:
    engine: {kind: graph, settings: {graph: ./graphs/researcher.json}}
    deps: {inference: infer}
    prepare:
      - type: memory.recall
        deps: {runtime: memory/runtime}
        settings: {into: hits, query: {board: query}, top_k: 8}
    commit:
      - type: memory.append
        deps: {runtime: memory/runtime}
        settings: {channel: __main_channel}
```

```yaml
# inference.yaml
version: v1
providers:
  - id: openai
    driver: openai
    profiles:
      - id: default
        operations: [embed]
        secrets:
          api_key: {resolver: env, key: OPENAI_API_KEY}
```

```yaml
# memory.yaml
version: v1
runtime:
  hard_partition: [runtime_id, user_id]
  default_scope: {runtime_id: prod}
  clock: {impl: system}
stores:
  messages: {impl: sqlite, settings: {file: ./memory/messages.db}}
  documents: {impl: sqlite, settings: {file: ./memory/documents.db}}
embedding:
  model:
    id: {provider: openai, name: text-embedding-3-small}
    profile: default
  dimensions: 1536
  batch_size: 32
  timeout: 30s
```

Credentials and provider catalogs stay in `inference.yaml`; `memory.yaml`
contains only the typed model reference and memory-side embedding policy.

The assembly also retains the scheduler lifecycle configuration:

```yaml
lifecycle:
  compact: {cron: "@hourly", older_than: 168h, keep: 50}
  archive:
    cron: "0 3 * * *"
    older_than: 2160h
    destination: s3://bucket/archive
```

The application registers `assembly.Runtime` and `assembly.Lifecycle` against a
shared `scheduler.Server` through `sdkx/scheduler/memory`. The backend-neutral
control, delivery, and lease contracts live in `sdk/scheduler`; the local cron
and queue implementation lives in `sdkx/scheduler`. In an application Runtime,
declare the Server as a deployment resource and reference it through
`runtime.scheduler`; Runtime starts a local Server after integrations register
their rules and workers. Plain `sdkx/deploy` still does not start background
maintenance itself.

`discard_on_interrupt` is the only built-in referee. It maps
`agent.DiscardOnInterruptCauses` to the run's `Committed` flag and is
the canonical disposition for voice / streaming UX.

## Delegation assembly

Local delegation joins deploy-built agents, an application-created
directory, execution-time tools, an optional asynchronous backend,
and a host capability. The directory must exist before `Build` because
the tool definitions and dynamic handoff referee capture it; bind it
to the successful result before serving requests.

Register the complete assembly in Go:

```go
directory := delegation.NewDirectory()

toolRegistry := tool.NewRegistry()
for _, delegationTool := range tooldelegation.New(directory) {
    toolRegistry.Register(delegationTool)
}
toolBuilder := toolconfig.NewBuilder(toolRegistry, toolconfig.Deps{})

engines := agent.NewRegistry()
engines.MustRegister(graphagent.NewFactory(graphagent.WithBaseDir(configDir)))

builder := deploy.NewBuilder(engines, deploy.WithBaseDir(configDir))
builder.MustRegisterResource(toolconfig.NewDeployFactory(toolBuilder))
builder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())
builder.MustRegisterResource(kanbanconfig.NewMemoryDeployFactory())
builder.RegisterReferee(
    delegationconfig.RefereeType,
    delegationconfig.NewHandoffRefereeFactory(directory),
)
```

The deployment exports the shared event bus and backend, binds the
backend to that bus, and enables the dynamic referee on agents that
may initiate handoff:

```yaml
version: v1
resources:
  events:
    kind: event.Bus
    impl: memory
    export: true
    settings:
      route_cache_size: 1024

  delegations:
    kind: delegation.AsyncBackend
    impl: kanban-memory
    export: true
    deps:
      event_bus: events
    settings:
      scope_id: local-delegation
      max_pending: 100
      max_cards: 1000
      card_ttl: 24h

  tools:
    kind: tool.Assembly
    impl: yaml
    settings:
      inline:
        version: v1
        middlewares:
          - kind: recover
          - kind: telemetry

agents:
  dispatcher:
    card:
      name: Dispatcher
      description: Routes work to local specialists
    tools: [delegate, delegation_status]
    engine:
      kind: graph
      settings:
        graph: ./graphs/dispatcher.json
    deps:
      tools: tools
    referees:
      - type: delegation_handoff

  researcher:
    file: ./agents/researcher.agent.yaml
```

After `Build`, bind and construct the runtime-facing pieces:

```go
result, err := builder.Build(ctx, document)
if err != nil {
    return err
}

if err := directory.Bind(result); err != nil {
    _ = result.Close()
    return err
}
backend, err := deploy.ResourceAs[delegation.AsyncBackend](result, "delegations")
if err != nil {
    _ = result.Close()
    return err
}
bus, err := deploy.ResourceAs[event.Bus](result, "events")
if err != nil {
    _ = result.Close()
    return err
}
baseHost := runtimeHost{bus: bus}
service, err := delegation.NewService(
    directory,
    backend,
    delegation.WithWorkerHost(baseHost),
)
if err != nil {
    _ = result.Close()
    return err
}

host := sdkdelegation.WithService(baseHost, service)
```

`sdkx/tool/delegation` obtains the service from the execution host.
`WithWorkerHost` gives asynchronous agents the same long-lived host
capabilities; the service adds its own delegation capability to that
host before running claimed work.
`delegate` returns a backend-neutral `delegation_id`; callers pass
only that ID to `delegation_status`. A direct synchronous call and an
agent execution use the same service and host:

```go
execCtx := agent.ContextWithHost(ctx, host)
syncResult, err := service.Delegate(execCtx, sdkdelegation.Request{
    Mode:   sdkdelegation.ModeSync,
    Target: "researcher",
    Input:  "Summarize the release notes",
})
if err != nil {
    return err
}

dispatcher, ok := result.Instance("dispatcher")
if !ok {
    return fmt.Errorf("dispatcher instance is not built")
}
turn, err := dispatcher.Execute(execCtx, request, agent.WithHost(host))
if err != nil {
    return err
}
if handoff, ok := sdkdelegation.HandoffFromResult(turn); ok {
    // The referee validated the target at decision time. Transfer the
    // application/session interaction to handoff.Target and preserve
    // handoff.Args.Input, Note, and Metadata.
}
```

The handoff event is structured data in `agent.Result.State`; deploy
does not transfer a session by itself. The application remains
responsible for routing the interaction and deciding what session
state to preserve.

Shutdown order is part of the ownership contract:

```go
// First stop delegation workers that borrow the backend and event bus.
if err := service.Close(); err != nil {
    return err
}
// Then close deploy-owned resources, including the backend and bus.
if err := result.Close(); err != nil {
    return err
}
```

`Service.Close` never closes the injected backend or its shared event
bus. Closing `Result` first can invalidate resources still used by
delegation workers.

## Event bus and the host

`event.Bus` is an **execution-time** Host capability, not a graph
build dep. The deployment builds it; the host hands it back to the
engine per turn.

```yaml
resources:
  events:
    kind: event.Bus
    impl: memory
    export: true
    settings:
      route_cache_size: 1024
```

```go
type runtimeHost struct {
    agent.NoopHost
    bus event.Bus
}

func (h runtimeHost) EventBus() event.Bus { return h.bus }

func (h runtimeHost) Publish(ctx context.Context, e event.Envelope) error {
    return h.bus.Publish(ctx, e)
}
```

```go
bus, err := deploy.ResourceAs[event.Bus](result, "events")
inst, _ := result.Instance("researcher")

response, err := inst.Execute(ctx, agent.Request{Message: userMessage},
    agent.WithHost(runtimeHost{bus: bus}),
)
```

The host owns execution behavior; `Result` owns the bus lifecycle.
`ResourceAs` returns a borrowed value — never close it yourself;
`defer result.Close()` will.

## Build and run

```go
engines := agent.NewRegistry()
engines.MustRegister(graphagent.NewFactory(graphagent.WithBaseDir(configDir)))

builder := deploy.NewBuilder(engines)

// Resources the document references.
builder.MustRegisterResource(workspaceconfig.NewDeployFactory())
builder.MustRegisterResource(sandboxconfig.NewDeployFactory())
builder.MustRegisterResource(inferenceyaml.NewDeployFactory(providerFactories, secretResolvers))
builder.MustRegisterResource(toolconfig.NewDeployFactory(toolBuilder))
builder.MustRegisterResource(jsrt.NewDeployFactory())
builder.MustRegisterResource(luart.NewDeployFactory())
builder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())

// Host-owned values the document can borrow.
builder.RegisterSource("host.memory", func(ctx context.Context, ref string) (any, error) {
    store, ok := memoryStores[ref]
    if !ok {
        return nil, errdefs.NotFoundf("memory store %q is not registered", ref)
    }
    return store, nil
})

data, err := os.ReadFile("deployment.yaml")
if err != nil { return err }
doc, err := deploy.Parse(data)
if err != nil { return err }

result, err := builder.Build(ctx, doc)
if err != nil { return err }
defer result.Close()

// Borrow shared resources the application needs.
bus, err := deploy.ResourceAs[event.Bus](result, "events")
if err != nil { return err }

// Run named agents.
inst, ok := result.Instance("researcher")
if !ok { return fmt.Errorf("researcher instance is not built") }

response, err := inst.Execute(ctx, agent.Request{Message: userMessage},
    agent.WithHost(runtimeHost{bus: bus}),
)
if err != nil { return err }
```

Construction order is topological from `ResourceEntry.Deps`. On
failure, already-built resources are closed before the error returns.
A successful `Build` produces a `Result` that owns every `io.Closer`
returned by a resource factory; values resolved through a `Source` are
never closed by `Result.Close`.

Shutdown when no delegation service borrows the result:

1. Stop request/session loops.
2. `defer result.Close()` from the function that called `Build` runs
   (or call it explicitly at shutdown).
3. `Close` is idempotent, runs in reverse construction order, joins
   all errors.

When a local delegation service exists, close it after stopping
request/session loops and before `Result.Close`, as shown in
[Delegation assembly](#delegation-assembly).

## Extending deploy

There are four extension points, each registered on the `Builder` you
already have:

| You want to add        | Method                                                      | Document surface                      |
| ---------------------- | ----------------------------------------------------------- | ------------------------------------- |
| A new kind of resource | `MustRegisterResource`                                      | a new `kind` / `impl` pair            |
| A new engine           | `agent.NewRegistry().MustRegister(...)`                     | a new `engine.kind`                   |
| A new hook kind        | `RegisterPreparer` / `RegisterReferee` / `RegisterCommitter` / `RegisterObserver` | a new `prepare/referees/commit/observe.type` |
| A host-owned value     | `RegisterSource`                                            | a new `source: <name>` dep ref        |

The four hook factory types are distinct so a factory registered
against the wrong stage is a compile error — there is no implicit
"if it has these methods it works":

```go
b.RegisterPreparer("seed", seedFactory)
b.RegisterReferee("policy", policyFactory)
b.RegisterCommitter("save", saveFactory)
b.RegisterObserver("audit", auditFactory)
```

Lifecycles are easy to test in isolation: each factory receives a
`HookInput` with the decoded settings and resolved deps, so unit tests
can hand a fixture `HookInput` and assert on the produced hook.

## Validation rules

Parsing and `Build` reject (all with structured errors, not panics):

- Unsupported document `version`.
- Unknown fields at any level (typos fail the build, never silently
  drop).
- Missing `kind` / `impl` on a resource.
- Factories, sources, engines, or hook types that are not registered.
- Dep names not declared by the consumer's `Spec` (resource or
  engine).
- Dep type mismatches (e.g. binding an `inference.Assembly` where a
  `workspace.Workspace` is declared).
- Item references (`name/item`) on resources that don't implement
  `ItemResolver`.
- Resource dependency cycles.
- Resources that nothing binds and that are not `export: true`
  or retained for an explicit external consumer (dead configuration).
- Unknown resource, engine, or hook `settings` keys.
- Typed-nil resources and dependencies.
- Invalid graph settings or graph node dependencies the engine
  doesn't recognize.

The first failed rule short-circuits the build, so the error is
specific to one offending value — not a cascade.

## Testing your deployment

A deployment document is plain data, so the easiest tests are
golden-file comparisons and built-document assertions:

```go
data, _ := os.ReadFile("testdata/deployment.yaml")
doc, err := deploy.Parse(data)
if err != nil { t.Fatal(err) }

result, err := builder.Build(ctx, doc)
if err != nil { t.Fatal(err) }
defer result.Close()

names := result.InstanceNames()
if !slices.Contains(names, "researcher") {
    t.Fatalf("expected researcher instance, got %v", names)
}

bus, err := deploy.ResourceAs[event.Bus](result, "events")
if err != nil { t.Fatal(err) }
```

For lifecycle hooks, exercise the factory directly with a hand-built
`HookInput` — the surrounding deploy machinery is irrelevant once the
factory is registered.

For full turn-level tests (the engine, the host, the hook chain),
see `sdkx/delegation/integration_test.go`, `tests/conformance`, and the
engine-specific packages (`sdkx/agent/graph`). The deploy layer itself is
hermetic: a `Build` over a parsed document does not perform network
I/O unless a resource factory does (e.g. an inference provider that
warms up its catalog on first use).

## Further reading

- Application runtime, sessions, ownership, and runtime configuration:
  [runtime.md](runtime.md).
- Package contracts: `sdkx/deploy/doc.go`, `sdkx/deploy/document.go`,
  `sdkx/deploy/builder.go`.
- Per-resource config schemas: `sdkx/workspace/config/doc.go`,
  `sdkx/sandbox/config/doc.go`, `sdkx/inference/config/doc.go`,
  `sdkx/tool/config/doc.go`, `sdkx/event/config/doc.go`,
  `sdkx/memory/config/doc.go`.
- Engine contract: `sdk/agent/doc.go`, `sdkx/agent/graph/factory.go`.
- Delegation contracts and local runtime: `sdk/delegation/doc.go`,
  `sdkx/delegation/doc.go`, `sdkx/tool/delegation/doc.go`.
- Lifecycle hooks: `sdk/agent/doc.go` (`Preparer`, `Referee`,
  `Committer`, `Observer`).
- A focused, on-disk example: `examples/forge` (native scenario workspaces
  driven by `sdkx/deploy` documents) and the inference guide's
  [deployment section](inference.md#deployment-configuration).
- Memory stack and the four integration surfaces
  (Preparer / Committer / Tool / Scheduler):
  [memory.md](memory.md).

`sdkx/deploy` remains an assembly layer, not a session runtime. It constructs
and owns resources and agent instances. When an application uses
`sdkx/runtime`, Runtime owns the deployment result and process lifecycle while
Session owns conversational execution; deploy owns neither sessions nor
transports.
