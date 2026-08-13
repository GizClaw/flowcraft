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
  The reserved `default` key is an optional fallback.
- Buffer and concurrency fields are validated against hard upper bounds.

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

## Host decorators

`runtime.Builder.WithHostFactory` wraps the base host factory. The decorator
must delegate any method it does not override.

See [deploy.md](deploy.md) and [resource.md](resource.md).
