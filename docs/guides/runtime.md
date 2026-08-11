---
layout: default
title: Application Runtime and Sessions
---
# Application Runtime and Sessions

`sdkx/runtime` is the transport-neutral application core above a built
deployment. It owns process-level services and exposes leased conversational
sessions. It does not implement HTTP, WebSocket, WebRTC, a CLI, or an
application-specific state protocol.

## Boundaries

| Layer | Responsibility | Does not own |
| --- | --- | --- |
| `sdkx/deploy` | Assemble named resources, hooks, engines, and agent instances into a `deploy.Result` | Sessions, turns, runtime services |
| `sdkx/runtime` | Build and own the deployment result, event router, integrations, and session manager | Conversation identity or transport connections |
| `sdkx/runtime/session` | Share sessions by `(AgentID, ContextID)` and run interruptible, streaming turns | Deployment resources or process-level services |

A Runtime owns its `deploy.Result`; callers never receive that result from the
Runtime API. Sessions and integrations only borrow deployment instances,
resources, hosts, and the event bus.

## Deployment configuration

`deploy.Parse` preserves the top-level `runtime` node as opaque data.
`runtime.Builder` then decodes that node strictly. Unknown runtime fields,
unknown integration kinds, undeclared dependencies, missing resources, kind
mismatches, Go type mismatches, and typed-nil resources fail construction.

The following `deploy.yaml` shows every runtime field and both first-party
integrations:

```yaml
version: v1

resources:
  events:
    kind: event.Bus
    impl: memory
    settings:
      route_cache_size: 1024

  schedules:
    kind: scheduler.Server
    impl: local

  checkpoints:
    kind: agent.CheckpointStore
    impl: sqlite
    settings:
      path: ./data/checkpoints.db

  ws:
    kind: workspace.Registry
    impl: yaml
    settings:
      file: ./workspace.yaml

  infer:
    kind: inference.Assembly
    impl: yaml
    settings:
      file: ./inference.yaml

  # Optional. Remove this resource and delegation's backend dep when only
  # synchronous local delegation is needed.
  delegations:
    kind: delegation.AsyncBackend
    impl: kanban-memory
    deps:
      event_bus: events
    settings:
      scope_id: local
      max_pending: 100
      max_cards: 1000
      card_ttl: 24h

  memories:
    kind: memory.Assembly
    impl: flowcraft
    deps:
      workspace: ws/project
      inference: infer
    settings:
      file: ./memory.yaml

agents:
  assistant:
    card:
      name: Assistant
      description: Handles interactive requests
    engine:
      kind: graph
      settings:
        graph: {file: ./graphs/assistant.json}

runtime:
  event_bus: events
  scheduler: schedules
  checkpoint_store: checkpoints
  sessions:
    idle_timeout: 10m
    sink_buffer: 256
    speculative_buffer_events: 1024
    speculative_buffer_bytes: 1048576
    resume: true
  integrations:
    - name: delegation
      kind: delegation.local
      deps:
        backend: delegations
      settings:
        max_concurrency: 4
        max_depth: 8
        timeout: "30s"
    - name: memory
      kind: memory.worker
      deps:
        memory: memories
      settings: {}
```

`event_bus`, `scheduler`, `checkpoint_store`, and every integration dependency
are exact resource map keys. Runtime never discovers resources by kind.
Runtime references are external consumers during deployment assembly, so
`events`, `schedules`, `checkpoints`, `delegations`, and `memories` do not
need `export: true`.

`checkpoint_store` names an `agent.CheckpointStore` resource (sqlite or
workspace-backed); `sessions.resume: true` enables checkpoint-based
resumption and requires it. Omit both for a stateless runtime.

`delegation.local` accepts `max_concurrency`, `max_depth`, and a Go-duration
`timeout` string. Its `backend` dependency is optional. The
`memory.worker` integration requires a `memory.Assembly`. Runtime
automatically injects the shared `scheduler.Server` named by
`runtime.scheduler` into integrations that declare that dependency, so it is
not repeated in each integration's `deps`. Its settings must be empty because
maintenance policy comes from the assembly's `memory.yaml` configuration
(derivation interval, scopes, and lifecycle settings).
Runtime starts the local server only after every integration has registered its
rules and started its leased worker. During shutdown, integrations stop their
workers before Runtime closes the server. A remote implementation can expose
the same `sdk/scheduler.Server` protocol without a local lifecycle.
Resources must not depend on the Server selected by `runtime.scheduler`;
Runtime owns its phased shutdown after integrations. The Server may still
depend on lower-level resources of its own.

## Register, build, and run

Register every implementation named by the document before building. The
application supplies the memory implementation factory and the shared tool
registry:

Aliases used below: `sdkscheduler` =
`github.com/GizClaw/flowcraft/sdkx/scheduler`, `schedulerconfig` =
`github.com/GizClaw/flowcraft/sdk/scheduler/config`, `sqlitecheckpointconfig` =
`github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite/config`, `workspaceconfig` =
`github.com/GizClaw/flowcraft/sdk/workspace/config`, `inferenceconfig` =
`github.com/GizClaw/flowcraft/sdk/inference/config`, `memoryconfig` =
`flowcraftmemory` =
`github.com/GizClaw/flowcraft/memory/config`, `flowcraftruntime` =
`github.com/GizClaw/flowcraft/memory/runtime`, `graphconfig` =
`github.com/GizClaw/flowcraft/sdk/graph/config`, `sdkconfig` =
`github.com/GizClaw/flowcraft/sdk/config`, and `message` =
`github.com/GizClaw/flowcraft/sdk/message`.

```go
loader := sdkconfig.NewLoader(sdkconfig.WithBaseDir(configDir))

deployBuilder := deploy.NewBuilder(deploy.WithLoader(loader))
deployBuilder.RegisterEngine(graphconfig.NewFactory(graphconfig.WithLoader(loader)))
deployBuilder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())

schedulerBuilder := schedulerconfig.NewBuilder()
if err := sdkscheduler.Register(schedulerBuilder); err != nil {
    return err
}
deployBuilder.MustRegisterResource(schedulerconfig.NewDeployFactory("local", schedulerBuilder))

deployBuilder.MustRegisterResource(sqlitecheckpointconfig.NewFactory())

deployBuilder.MustRegisterResource(kanbanconfig.NewMemoryDeployFactory())

workspaceBuilder := workspaceconfig.NewBuilder(workspaceconfig.Deps{BaseDir: configDir})
deployBuilder.MustRegisterResource(workspaceconfig.NewDeployFactory(workspaceBuilder))
deployBuilder.MustRegisterResource(inferenceconfig.NewDeployFactory(providerFactories, secretResolvers))
deployBuilder.MustRegisterResource(flowcraftmemory.Factory())

tools := tool.NewRegistry()
delegationFactory, err := delegationruntime.NewFactory(tools)
if err != nil {
    return err
}

runtimeBuilder := runtime.NewBuilder(deployBuilder)
if err := runtimeBuilder.RegisterIntegration(delegationFactory); err != nil {
    return err
}
if err := runtimeBuilder.RegisterIntegration(flowcraftruntime.NewFactory()); err != nil {
    return err
}

data, err := os.ReadFile("deploy.yaml")
if err != nil {
    return err
}
document, err := deploy.Parse(data)
if err != nil {
    return err
}
app, err := runtimeBuilder.Build(ctx, document)
if err != nil {
    return err
}
defer app.Close()
```

Open a lease, attach sinks before execution starts, and wait independently of
the turn:

```go
lease, err := app.Sessions().Open(ctx, session.Key{
    AgentID:   "assistant",
    ContextID: conversationID,
})
if err != nil {
    return err
}
defer lease.Close()

turn, err := lease.Session().Start(ctx, agent.Request{
    Message: message.NewTextMessage(message.RoleUser, text),
}, session.SinkSpec{
    ID:   connectionID,
    Sink: streamSink,
    OnDetach: func(err error) {
        log.Printf("stream detached: %v", err)
    },
})
if err != nil {
    return err
}

result, err := turn.Wait(ctx)
if err != nil {
    return err
}
```

`Session.Start` always replaces a caller-supplied `Request.ContextID` with the
session key and replaces `Request.RunID` with the session's root RunID — fresh
per turn by default, stable across turns when `sessions.resume` is enabled.
Use `turn.RunID()` for event correlation.

Prompt requests arrive through the same event stream as
`session.PromptRequested`. Reply with the correlated prompt ID:

```go
if err := turn.Reply(ctx, prompt.PromptID, agent.UserReply{
    Parts: []message.Part{message.TextPart{Text: answer}},
}); err != nil {
    return err
}
```

Interrupts are cooperative and idempotent. Starting a replacement turn in the
same Session interrupts and fully finalizes the previous turn first:

```go
_ = turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserCancel})
result, err := turn.Wait(ctx)
```

Canceling the context passed to `Start` cancels execution. Canceling a context
passed only to `Wait` cancels that waiter, not the turn; another caller may
wait again.

At application shutdown, close leases when their consumers leave and close the
Runtime once:

```go
_ = lease.Close()
if err := app.Close(); err != nil {
    return err
}
```

After Runtime close begins, the manager rejects new leases and turns.

## Integration factory lifecycle

An `IntegrationFactory` participates in five ordered phases:

1. `Prepare` runs before deployment build. It may decode integration settings
   and register factories, sources, or tools, but must not start goroutines or
   use deployment resources.
2. `Bind` runs after a successful deployment build. It receives only declared,
   validated borrowed dependencies, a read-only deployment view, and the base
   host factory.
3. `DecorateHost` wraps the per-turn `session.HostFactory`. It must preserve
   capabilities required by inner hosts, including `EventBus`.
4. `Start` begins integration-owned service work after the session manager is
   ready.
5. `Close` stops integration-owned work without closing borrowed resources.

Construction is transactional. On failure, integrations with build-time side
effects first roll them back in reverse order; all prepared integrations then
close, followed by the deployment result. Normal shutdown preserves installed
scheduler rules while stopping workers before the shared Server. Runtime
caches the aggregate close result for repeated or concurrent callers.

## Streams, prompts, and partial output

Each `SinkSpec` has an independent bounded queue. `QueueSize: 0` uses
`runtime.sessions.sink_buffer`. A full queue or sink error detaches only that
sink and invokes `OnDetach`; it does not stop the turn or sibling sinks.
Initial sinks are attached before the engine can publish. `DeliveryTimeout`
bounds each transport write; zero uses the 30-second default. A timed-out sink
is detached so it cannot block turn or Runtime shutdown.

The zero-value `SinkSpec` remains a raw observer with delivery-time ACK
semantics. Raw sinks receive every event immediately, including speculative
branch output, and their envelopes do not carry delivery cursors. Set
`Visibility: session.VisibilityConfirmed` to buffer speculative output until
the graph accepts its `(ForkID, BranchID)`. Confirmed sinks receive the accept
control event followed by the branch events in original order; cancellation
drops the buffered events. Their cloned envelopes carry
`session.HeaderDeliveryCursor`, readable with
`session.DeliveryCursorFromEnvelope`. Cursors are contiguous and shared by all
confirmed sinks in a turn.

At most one confirmed sink may set
`Authority: session.AuthorityAuthoritative`. With the default
`AckMode: session.AckOnDelivery`, a successful `OnDelta` acknowledges its
cursor automatically. With `AckMode: session.AckExplicit`, the transport calls
`turn.Ack(sinkID, cursor)` cumulatively. `MaxUnacked: 0` uses the sink's
effective queue size. An interrupt freezes the acknowledged token prefix
before the interrupt reaches the engine. If the engine returns interrupted,
that prefix—not the engine's unacknowledged suffix—is exposed to Committers;
the original `Turn.Wait` result is unchanged. A deployed Referee that discards
the result still takes precedence.

When a Referee requests a revise, confirmed delivery cursors remain
turn-global and cumulative, but commit authority moves to the new attempt.
Previously acknowledged text is cleared, and delayed delivery or ACK activity
from an older attempt may advance the cursor without entering the new
attempt's committable prefix.

Engine or Graph `run.end` events delimit internal attempts and are not exposed
to session sinks. Raw and confirmed sinks each receive one synthetic
`run.end` only when the logical turn finishes, after all attempts. If an
attempt leaves a speculative branch unresolved, confirmed sinks detach with a
conflict while raw sinks still receive the logical end. A Graph run-end
publication failure remains an error path and does not produce a synthetic
success.

Speculative buffering is aggregated once per turn, not once per sink. The
defaults are 1,024 events and 1 MiB and can be changed with
`runtime.sessions.speculative_buffer_events` and
`runtime.sessions.speculative_buffer_bytes`. Both must be positive. A protocol
conflict or limit overflow detaches confirmed sinks while raw sinks and engine
execution continue.

One turn may have multiple concurrent `AskUser` calls. Every request has a
distinct `PromptID` and carries the turn's RunID. Unknown, duplicate, expired,
interrupted, and closed replies return deterministic errors. A transport should
route replies by both Turn/RunID and PromptID.

Interrupted output is not committed by default. Without an authoritative sink,
delivery progress has no durable meaning and existing behavior is unchanged. A Referee may return
`agent.Decision{AcceptOutput: true}` to send engine-materialized partial output
through the normal Committer chain. `DiscardOutput` always wins over
`AcceptOutput`. Session never writes partial output directly.

## Tools and application state

MCP remains a tool protocol. MCP-projected tools and ordinary tools continue
through the existing `tool.Registry` and deployment tool assembly; Runtime and
Session add no private tool-result RPC or second tool channel. The
`delegation.local` integration uses the same registry and exposes its service
as a host capability.

Runtime and Session do not persist arbitrary `agent.Board` variables or whole
Board snapshots. Stateful applications must explicitly load and save domain
state through workspace-capable graph nodes or another declared persistence
integration.

The first runtime release intentionally defers HTTP, SSE, WebSocket, WebRTC
signaling, and WebRTC DataChannel adapters. Those transports may adapt
`Runtime`, `Session`, `Turn`, events, prompts, and sinks without changing
their semantics.
