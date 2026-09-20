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
// One OpenAI-wire driver serves the whole family: register it once per
// deployment document, next to the other provider drivers you use.
reg.MustRegister(openai.Factory())

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
instead, or use the external dependency channel below.

### External dependencies

`runtime.external_deps` declares caller-owned dependency values that are
built by the application and only consumed as resource/engine/hook
dependencies:

```yaml
runtime:
  event_bus: events
  external_deps:
    - name: db
      contract: db.Pool
  sessions:
    idle_timeout: 10m
```

The application injects the matching value before building:

```go
builder := runtime.NewBuilder(reg)
if err := builder.WithExternalResource(runtime.ExternalResource{
    ExternalDependency: runtime.ExternalDependency{
        Name:     "db",
        Contract: "db.Pool",
    },
    Value: db,
}); err != nil {
    return err
}
app, err := builder.Build(ctx, doc)
```

Any resource, agent engine, or hook can then reference `db` in its
`deps`; the consuming factory must declare `{name: db, type: db.Pool}`
in its `resource.Spec.Deps` so deploy can check the contract.

External values are borrowed, not owned:

- They never participate in `Wire` / deployment binding, `Close`,
  `Runtime.Drain`, or dynamic agent rebinding lifecycle.
- They are not rebuilt or replaced by `Reload`; every generation
  receives the same injected object.
- They are hidden from `Runtime.Resource` and `Result.Names` — they
  exist only as dependencies.
- `Reload` may shrink the declared set, but may not require an external
  value that was not injected when the Runtime was built.
- The application is responsible for closing external values after all
  Runtimes sharing them are closed.

External dependencies cannot serve runtime-managed roles: `event_bus`,
`checkpoint_store`, and `dynamic_catalog.tools` resource names must be
regular deployment resources.

## Runtime config

```yaml
runtime:
  event_bus: events
  checkpoint_store: checkpoints   # optional
  external_deps:                  # optional
    - name: db
      contract: db.Pool
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

While a turn runs, `Turn.Interrupt` stops it at the next safe checkpoint
and `Turn.Steer` hands it a message that a document node delivers at a
boundary the document chose — see
[Steering a running turn](#steering-a-running-turn).

Delegated subagent sessions inherit the caller turn's stream sinks: the
turn execution context carries the caller's stream policy, and the
delegation service attaches those sinks to subagent runs as observers
(authority and explicit-ack semantics are downgraded because the sink has
no handle to the subagent turn). An inherited sink may be invoked
concurrently from multiple sessions and must be safe for concurrent
`OnDelta` calls.

### DeleteSession

`app.Sessions().DeleteSession(ctx, key)` removes one session's durable
state by key (agent id + context id) and closes its live session — the
by-key counterpart of `UnregisterAgent` for delete/archive-a-conversation
workflows. After it returns, the checkpoint store no longer carries the
key's committed history, parked-run checkpoint, or resumable request; a
later `Open` starts with empty history. Semantics mirror removal: new
opens for the key are refused until deletion finishes, the live session is
drained (bounded by ctx) before store entries are removed, and on ctx
expiry the delete marker rolls back with no partial removal — the call is
retryable and repeated calls are idempotent.

### Draining a runtime offline

`Runtime.Drain(ctx)` quiesces a runtime whose object is about to be
replaced (for example a blue-green rebuild that `Reload` cannot express):
new session leases and new turns on already-open leases are refused, and
the runtime waits (bounded by `ctx`) for active turns to finish naturally.
It never interrupts running turns. After `Drain` returns, the runtime stays
drained; retry a timed-out drain or proceed to `Close` once the replacement
is ready:

```go
if err := old.Drain(ctx); err != nil {
    return err // runtime stays drained; retry or force Close
}
if err := old.Close(); err != nil {
    return err
}
newApp, err := runtime.NewBuilder(reg).Build(ctx, doc)
```

The session manager exposes the same primitives for embedded use:
`Manager.Idle()` reports whether any session still has active turns,
prompts, or sinks; `Manager.WaitIdle(ctx)` waits without changing state;
`Manager.Drain(ctx)` quiesces and waits. Prefer `Runtime.Drain` when the
application owns a `Runtime`, since it serializes against
`RegisterAgent` / `UnregisterAgent` / `Reload` / `Close`.

## Steering a running turn

`Turn.Steer(msg)` hands a message to a turn that is already running — the
inbound counterpart of `Turn.Interrupt`, which can only stop a run, never
correct it. The queue belongs to the turn (not to the host factory, not to
the deployment document): `Turn.PendingSteer()` reports what is queued,
`Turn.DrainSteer()` takes everything queued so far, and the session installs
the turn's queue as the `agent.SteerSource` capability on every turn host.
Documents read it where they choose, through the script global
`host.drainSteer()` (see [graph.md](graph.md)); core never injects text
mid-stream, and the graph executor's interrupt checkpoints are not delivery
points.

`Steer` always reports non-delivery instead of silently dropping:

- `ErrSteerQueueFull` (budget exceeded) once 8 messages are queued — the
  queue is a low-latency correction channel, not a second inbox, so the
  caller keeps its message and queues it for the next turn;
- `ErrSteerTooLarge` (budget exceeded) for one oversized message;
- `agent.Interrupted(...)` once an interrupt was requested: the remaining
  waves are not guaranteed to run, so accepting would promise a delivery
  that cannot happen;
- `ErrSteerClosed` (not available) once the turn is terminal.

Draining is not delivery: a message a node drained and then failed to use
(or that a checkpoint rollback left outside the resumed board) is gone.
Undelivered messages are therefore observable two ways — `PendingSteer()`
while the turn lives, and the `session.pending_steer` key on the turn result
state for a turn that ended with messages still queued (the messages stay
retrievable through `DrainSteer`). Steer queues are turn-scoped and never
persisted, so a resumed run starts empty.

The event stream does not carry that count. The run-end envelope is published
from inside `Execute` (`core/graph/execute.go`), which returns before the
turn settles and records the key, and nothing re-publishes afterwards — so an
embedder that only subscribes to the bus detects non-delivery by reading the
turn result (`Turn.Wait` or the application's own result path), where the
value is an `int` in process and a JSON number after a round trip.

Because one turn has exactly one queue, a `WithHostFactory` /
`WithResultHostFactory` product that already implements `agent.SteerSource`
fails the start with a conflict rather than being silently shadowed.

Delegated sub-runs are then a per-case story rather than a blanket "not
steerable": a sub-run with its own turn (delegation against a bound
`session.Manager`) has its own queue — steerable by whoever starts that turn,
not by the parent; a sub-run with no turn of its own is not steerable at all,
and in legacy mode (no manager bound) the synchronous path inherits the
caller's Host, where the delegation service withholds `agent.SteerSource`
from what the child engine receives, so a delegated steer node cannot drain
the parent's queue. See [delegation.md](delegation.md).

The session always wraps the turn Host with its steer source (outermost, on
top of the ephemeral wrapper), so the Host an engine receives is never the
factory product itself: optional capabilities are reachable through
`agent.CapabilityFromHost` — `agent.SteerFromHost`, `agent.EventBusFromHost`
and `delegation.ServiceFromHost` are the facades over it — never by asserting
the concrete Host type.

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
