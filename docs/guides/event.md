---
layout: default
title: Event Bus
---
# Event Bus Guide

`core/event` is a subject-routed publish/subscribe bus. The in-process
implementation is `MemoryBus`; remote implementations may implement the same
`Bus` interface.

## Core types

| Type | Role |
| --- | --- |
| `event.Bus` | `Publish`, `Subscribe`, `Close` |
| `event.Envelope` | id, subject, time, headers, payload, trace IDs |
| `event.Subject` | dot-delimited routing key |
| `event.Pattern` | `*` matches one segment; `>` matches one or more trailing |

## Subscribe and publish

```go
bus := event.NewMemoryBus()

sub, err := bus.Subscribe(ctx, "graph.run.*.start")
if err != nil {
    return err
}
defer sub.Close()

env, _ := event.NewEnvelope(ctx, "graph.run.r1.start", payload)
if err := bus.Publish(ctx, env); err != nil {
    return err
}
```

`MemoryBus` has a subject-route cache and supports `DropNewest`,
`DropOldest`, `Block`, and `Sample` backpressure. Buffer size is capped.

## Deployment resource

```yaml
resources:
  events:
    kind: event.Bus
    impl: memory
    settings:
      route_cache_size: 1024  # optional: positive caps the subject route cache, zero disables it
```

The runtime host exposes the bus through `agent.EventBusProvider`.
Runtime-level consumers can also subscribe through `Runtime.Attach`
without resolving the resource themselves; the prompt lifecycle events
are documented in [prompt.md](prompt.md). Runtime agent lifecycle events
(`runtime.agent.<id>.registered` / `.removed`, subscribe with
`PatternAgentLifecycle()`) live in a separate namespace and are published
on successful dynamic registration/removal, and generation reloads publish
`runtime.rebuild.started` / `.completed` / `.failed` (`PatternRuntimeRebuild()`)
— see [runtime.md](runtime.md).
