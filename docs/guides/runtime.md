---
layout: default
title: Application Runtime and Sessions
---
# Application Runtime and Sessions

`core/runtime` owns a built deployment and exposes transport-neutral sessions.
It does not implement HTTP, WebSocket, WebRTC, a CLI, or an application
protocol.

## Build

Create a registry, register every referenced resource factory, then build:

```go
reg := resource.NewRegistry()
reg.MustRegister(event.NewFactory())
reg.MustRegister(graphresource.Factory())
reg.MustRegister(driverDeepSeek.Factory())

app, err := runtime.NewBuilder(reg).Build(ctx, doc)
if err != nil {
    return err
}
defer app.Close()
```

`Runtime` owns its `deploy.Result`, event router, and session manager.
`Close` releases the deployment (resources and deployed agents), the
session manager, and any dynamically registered agents.

## Subscribing to runtime events

`Runtime.Attach` subscribes a sink to the runtime's event router
without resolving the deployment document's `event_bus` resource:

```go
detach, err := app.Attach(ctx, session.PatternPromptRequested(), sink)
if err != nil {
    return err
}
defer detach()
```

Attachments are torn down when the Runtime closes, and `Attach` fails
with `NotAvailable` afterwards. External attachments inherit the bus
default backpressure (`DropNewest`); pass
`event.WithAttachBackpressure` to override per subscription. See
[prompt.md](prompt.md) for the prompt lifecycle events, and the
"Dynamic agent registry" section below for the `runtime.agent.*`
lifecycle events published on dynamic registration/removal.

## Accessing deployment resources

`Runtime.Resource` borrows a built resource value by its deployment
name from the current generation, mirroring `Agent` for the resource
view:

```go
db, ok := app.Resource("db")
if !ok {
    return errors.New("deployment has no db resource")
}
```

Values are borrowed: the Runtime owns the deployment and closes
resources when it closes, and a `Reload` retires the previous
generation's values. Use this for access to deployment-built services
such as a database pool. If the application must own a value's
lifecycle or keep it across reloads, construct it outside the runtime
and inject it through a resource factory registered in the registry
instead.

## Runtime config

```yaml
runtime:
  event_bus: events
  checkpoint_store: checkpoints   # optional
  sessions:
    idle_timeout: 10m
    sink_buffer: 256
    delivery_concurrency: 8
    speculative_buffer_events: 1024
    speculative_buffer_bytes: 1048576
    max_sessions: 1024
    resume: false
  dynamic_catalog:
    tools:
      default: shared_tools
      researcher: research_tools
```

Rules:

- `event_bus` is required and must name an `event.Bus` resource.
- `checkpoint_store` is optional; it names an `agent.CheckpointStore`.
- `sessions.resume` requires `checkpoint_store`.
- `dynamic_catalog.tools` maps agent IDs to `tool.Assembly` resources.
  The reserved `default` key is an optional fallback. The mapping is
  live: dynamically registered agents may attach a tool assembly at
  registration time (see below). Every mapping key must name a deployed
  agent — an unknown key fails the build (`no such deployed agent`).
- Buffer and concurrency fields are validated against hard upper bounds.

## Checkpoint stores

`checkpoint_store` names a resource implementing `agent.CheckpointStore`.
The core backend is the workspace store:

```yaml
resources:
  cps:
    kind: checkpoint.Store
    impl: workspace
    deps:
      workspace: ws
    settings:
      prefix: agent/checkpoints  # optional; default "agent/checkpoints"
```

Checkpoint files live under the workspace's `prefix` directory.
`sessions.resume: true` requires a store, and reloads additionally require
it to implement `agent.CheckpointDeleter` (the core workspace store does).
Concrete alternative backends are app-registered outside `core/` and own
their settings schema.

## Sessions

```go
lease, err := app.Sessions().Open(ctx, session.Key{
    AgentID:   "assistant",
    ContextID: "conversation-1",
})
if err != nil {
    return err
}
defer lease.Close()

turn, err := lease.Session().Start(ctx, agent.Request{
    Message: message.NewTextMessage(message.RoleUser, "hello"),
}, session.SinkSpec{ID: "console", Sink: streamSink})
if err != nil {
    return err
}

result, err := turn.Wait(ctx)
```

`SinkSpec` controls streaming delivery, visibility, authority, and queue
size. `delivery_concurrency` bounds in-flight sink callbacks.

## Dynamic agent registry

Agents are normally declared in the deployment document and fixed for the
life of the `Runtime`. `Runtime` also exposes a live registry so agents
can be registered and removed at runtime without rebuilding:

```go
instance, err := app.RegisterAgent(ctx, "qa", agent.Definition{
    Card:   agent.AgentCard{Name: "Ticket QA"},
    Engine: agent.EngineRef{Kind: "agent.Engine", Impl: "graph"},
}, runtime.WithToolAssembly("shared_tools")) // optional tool catalog
if err != nil {
    return err
}

lease, err := app.Sessions().GetOrCreate(ctx, session.Key{
    AgentID: "qa", ContextID: "user-7",
})
// ... Start / Wait, exactly like a deployed agent

if err := app.UnregisterAgent(ctx, "qa",
    runtime.WithRemoveTimeout(30*time.Second)); err != nil {
    return err
}
```

Semantics:

- `RegisterAgent` runs the `Definition` through the same assembly path as
  deployment (`deploy.BindAgent`: engine factory, dependency resolution,
  hook construction and wiring). A name that collides with a deployed or
  already-registered agent is a `Conflict`; assembly failures are
  `Validation` and never leave a partial registration.
- `UnregisterAgent` blocks new sessions for the agent, waits for active
  turns to finish naturally (bounded by the caller context or
  `WithRemoveTimeout`), then closes the agent's engine and hooks. On
  timeout the agent stays registered and sessions stay intact; the call
  is retryable. Unknown names are an idempotent no-op; deployed agents
  cannot be removed at runtime (`Conflict`).
- `Agent` / `AgentNames` are the live view: dynamically registered agents
  plus the deployment snapshot.
- With `dynamic_catalog` configured, a registration must either carry
  `WithToolAssembly(<resource name>)` or be covered by the `default`
  assembly — the same rule the build enforces for deployed agents.
- Every successful register/remove publishes a lifecycle event under
  `runtime.agent.<id>.registered` / `.removed` (subscribe with
  `PatternAgentLifecycle()`); the payload carries `agent_id`, `name`, and
  `description`.

`Manager.RemoveAgent` / `Manager.ReopenAgent` are the session-manager
level primitives behind removal and re-registration.

## Reload

`Runtime.Reload` transactionally replaces the deployment document without
rebuilding the runtime:

```go
result, err := app.Reload(ctx, newDoc)
if err != nil {
    // the previous generation keeps serving
    return err
}
_ = result.GenerationID
```

Semantics:

- The new generation is built, validated, and swapped atomically; any
  failure aborts before the swap and the current generation stays in
  service. In-flight turns always complete on the generation they started
  on; the next `Start` uses the new generation.
- Each generation owns its `event_bus` / `checkpoint_store` values, and
  the router subscribes to every live generation's bus, so the document
  may change their configuration or implementation freely. Continuity of
  durable session state across a store change is the host's
  responsibility (same backing storage or migration). A reload whose
  `event_bus` factory returns the current generation's bus (a shared
  singleton) is rejected.
- Dynamically registered agents are re-bound against the new result;
  agents removed by the new document have their sessions drained before
  the swap. `Reload` is serialized with `RegisterAgent` /
  `UnregisterAgent` / `Close`.
- Progress is published as `runtime.rebuild.started` / `.completed` /
  `.failed` events (`SubjectRuntimeRebuild*`, subscribe with
  `PatternRuntimeRebuild()`); the `RuntimeRebuildEvent` payload carries
  `generation_id`, `previous_generation_id`, `rebound_agents`,
  `drained_agents`, and `error` (on failure). See [event.md](event.md)
  for the event namespaces.

## Host decorators

`runtime.Builder.WithHostFactory` wraps the base host factory. The decorator
must delegate any method it does not override.

`runtime.Builder.WithResultHostFactory` wraps the factory a second time with
access to the fully assembled deployment, after `WithHostFactory` has run.
This is the seam for deployment-built, run-scoped services: applications opt
in and decide which services to expose on every turn host, and the runtime
itself stays neutral to them. For example, exposing a delegation service:

```go
builder.WithResultHostFactory(func(result *deploy.Result, factory session.HostFactory) (session.HostFactory, error) {
    return hostwrap.Wrap(factory, result) // delegation.Service onto every turn host
})
```

The decorator is retained across reloads and re-applied to each new
generation's host factory with that generation's deployment.

See [deploy.md](deploy.md) and [resource.md](resource.md).
