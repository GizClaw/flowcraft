---
layout: default
title: Event Bus
---
# Event Bus Guide

`sdk/event` is a subject-routed publish/subscribe bus for cross-process-friendly
envelopes. The single-process implementation (`MemoryBus`) and a
no-op bus (`NoopBus`) ship in the box; remote implementations (NATS,
Kafka, …) plug into the same `Bus` interface.

A bus is a **host capability**, not a graph build dep. Deployments
build it, then surface it through `agent.Host.EventBus()` per turn —
the engine asks the host for the bus it needs. See
[deploy.md#event-bus-and-the-host](deploy.md#event-bus-and-the-host)
for the wiring.

## Concepts

### Bus, Envelope, Subject, Pattern

| Type             | Role                                                                |
| ---------------- | ------------------------------------------------------------------- |
| `event.Bus`      | the contract: `Publish`, `Subscribe`, `Close`                       |
| `event.Envelope` | the wire value: id, subject, timestamp, headers, payload, trace IDs |
| `event.Subject`  | dot-delimited routing key (e.g. `graph.run.r1.node.n1.complete`)    |
| `event.Pattern`  | NATS-style matcher (`*` one segment, `>` one or more trailing)      |

Subjects and patterns share grammar; a `Pattern` matches a `Subject`
when each segment matches (`*` matches exactly one, `>` only allowed
as the last segment).

### In-process vs remote

`MemoryBus` is the in-process bus. It is goroutine-safe, has a
subject-to-subscriber route cache (memoised match per subject,
invalidated on subscribe / unsubscribe / close), and is the
implementation most SDK code targets.

`NoopBus` discards every `Publish` and returns a subscription whose channel
is already closed (a nil error from `Subscribe`). Use it in tests, hot paths
that don't need to be observable, or as a placeholder when the host is
constructed before the bus is.

Remote buses (NATS, server-side SSE, durable queues) implement the
same `Bus` interface. `Envelope` is JSON-shaped, with the payload
held as `json.RawMessage` so in-memory and remote paths behave
identically (bytes in, bytes out — no `any`-typed surprises).

## First bus

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"

    "github.com/GizClaw/flowcraft/sdk/event"
)

func main() {
    bus := event.NewMemoryBus()
    defer bus.Close()

    sub, err := bus.Subscribe(context.Background(), "graph.run.*.complete", event.WithBufferSize(64))
    if err != nil { panic(err) }
    defer sub.Close()

    go func() {
        for env := range sub.C() {
            fmt.Println(string(env.Payload))
        }
    }()

    _ = bus.Publish(context.Background(), event.MustEnvelope(
        context.Background(),
        "graph.run.r1.node.n1.complete",
        json.RawMessage(`{"ok":true}`),
    ))
}
```

`Subscribe` returns a `Subscription` whose `C()` channel is
the deliver path. Closing the subscription (or the bus) closes the
channel. The bus stops accepting publishes after `Close`.

## Envelope

```go
type Envelope struct {
    ID        string            // xid by default; server-stable
    Subject   Subject           // routing key
    Time      time.Time         // producer-side timestamp
    Source    string            // optional producer locator
    Headers   map[string]string // well-known keys (see below)
    Payload   json.RawMessage   // opaque to the bus
    TraceID   string            // OTel trace ID (hex)
    SpanID    string            // OTel span ID (hex)
}
```

Construction goes through `event.NewEnvelope(ctx, subject, payload)` (allocates
a fresh xid, stamps `Time`, and fills OTel trace/span IDs from `ctx`) or
`event.MustEnvelope` (panics on error, for static literals). Decoding uses the
`Envelope.Decode(&out)` method to unmarshal into a typed Go value — the bus
never decodes it for you.

The well-known header keys are constants in the package
(`event.HeaderRunID`, `event.HeaderNodeID`, `event.HeaderAgentID`,
`event.HeaderGraphID`, `event.HeaderTenant`); tooling should treat unknown
headers as opaque.

## Subject and Pattern

```go
sub1, _ := bus.Subscribe(ctx, "graph.run.*.complete", ...)        // any run
sub2, _ := bus.Subscribe(ctx, "graph.run.r1.>", ...)              // run r1, all subjects
sub3, _ := bus.Subscribe(ctx, "graph.run.r1.node.n1.complete", ...)  // exact
sub4, _ := bus.Subscribe(ctx, "*.*.>", ...)                       // two leading segs, any tail
```

A pattern's `*` matches one segment, never more and never zero. `>`
must be the last segment and matches one or more trailing segments.
Patterns with malformed segments (containing `.`, `*`, or `>` in the
middle) fail at `Subscribe` time.

## Backpressure

Each subscription carries a bounded buffer. When `Publish` arrives
and the buffer is full, the subscription's `BackpressurePolicy`
decides:

| Policy                 | Behaviour                                                                |
| ---------------------- | ------------------------------------------------------------------------ |
| `DropNewest` (default) | incoming envelope is dropped; older items stay                           |
| `DropOldest`           | oldest buffered item is dropped; new one is enqueued                     |
| `Block`                | `Publish` blocks until the buffer has room or the publishing ctx cancels |

Drop policies are fast paths — `Block` is the only one that can
back-pressure publishers. Use `Block` only when a slow subscriber is
expected to catch up and a slow publisher is acceptable; otherwise
default to `DropNewest` and observe drop counts via the `Observer`.

## Host integration

`agent.Host` declares an `EventBus() event.Bus` capability. Engines
(such as the graph runner) call this per turn to publish lifecycle
events; the host owns the bus and may share one bus across many
runs or give each run its own.

```go
type runtimeHost struct {
    agent.NoopHost
    bus event.Bus
}

func (h runtimeHost) EventBus() event.Bus { return h.bus }
```

The simplest wiring is to expose the bus verbatim. Decorators can
insert tracing, redaction, or per-tenant isolation between the
engine and the bus.

## Deploy integration

`event.Bus` is a first-party resource in deployment documents. Mark
it `export: true` so the application can retrieve it through
`deploy.ResourceAs[event.Bus](result, "events")` and hand it to the
host:

```yaml
resources:
  events:
    kind: event.Bus
    impl: memory
    export: true
    settings:
      route_cache_size: 1024
```

`impl: memory` is the only in-process impl; remote impls ship in
their own packages and register with the deploy builder. Unknown
impls fail the build with `<name> is not registered` — the same
error surface every other deploy extension point uses.

## Observing a bus

`MemoryBus` accepts an `Observer` for lifecycle instrumentation —
durations, drop counts, subscriber count, and the route cache
hit/miss ratio. Replace it with your own `Observer` implementation
to forward to Prometheus, OTel, or a custom metric sink. Drops and
backpressure events are first-class signals here, not log noise.

## Testing

| Test need                                           | Use                                                                    |
| --------------------------------------------------- | ---------------------------------------------------------------------- |
| No-op bus in hot paths / placeholder hosts          | `event.NoopBus{}`                                                      |
| In-process bus, want to assert publishes            | `event.NewMemoryBus()` and read from the subscription channel          |
| Need a fast deterministic clock or traced envelopes | construct `Envelope{TraceID, SpanID}` by hand and pass through the bus |

The bus is hermetic: a `MemoryBus` does not perform network I/O.
For remote implementations, the conformance suite lives in the
per-package test files (NATS / Kafka / …) and is held to the
shared `Bus` contract.

## Further reading

- Package contract: `sdk/event/bus.go` (the `Bus` / `Subscription`
  / `SubOption` surface), `sdk/event/envelope.go`, `sdk/event/subject.go`,
  `sdk/event/observer.go`, `sdk/event/trace.go`.
- Host capability that reaches it: `sdk/agent/host.go`
  (`Host.EventBus`).
- Assembly: `sdk/event/config/resource.go`, the `event.Bus` resource
  in [deploy.md](deploy.md#first-party-impls).
- Remote bridges: per-package `doc.go` (NATS / SSE / …).
