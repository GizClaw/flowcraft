# Runtime section schema (`deploy.yaml` -> `runtime:`)

Owned by `core/runtime`. `deploy.Parse` preserves the subtree opaquely;
`runtime.DecodeConfig` decodes it strictly. `event_bus` is required when a
`runtime` section exists (the validator enforces this).

```yaml
runtime:
  event_bus: events
  checkpoint_store: checkpoints       # optional
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

## Rules

- `event_bus` must name a resource whose built value implements the
  `event.Bus` contract (validator: field required; host build: resolves
  the name against the built resources).
- `checkpoint_store`, when present, must name a resource whose built
  value implements the `agent.CheckpointStore` contract — the document
  kind is `checkpoint.Store` (workspace) in core; alternative backends
  are app-registered outside `core/` (host build resolves it; the
  validator checks `resume` rules).
- `sessions.resume: true` requires `checkpoint_store` — **validator**.
- `sessions.sink_buffer`, `delivery_concurrency`,
  `speculative_buffer_events`, and `speculative_buffer_bytes` are validated
  against hard upper bounds — **validator**.
- `sessions.max_sessions` limits distinct live session keys.
- `dynamic_catalog.tools` maps agent IDs to `tool.Assembly` resources; the
  reserved `default` key is an optional fallback. Every deployed agent must
  be mapped directly or covered by `default` (host build/runtime
  registration; the validator only checks the map is non-empty). Mapping
  keys must name deployed agents — an unknown key fails the build
  (`no such deployed agent`). The mapping is live: agents registered at
  runtime attach a tool assembly via `WithToolAssembly` (see below) or
  fall back to `default`.

## Dynamic agent registration (core/v0.1.11+)

Agents can be registered and removed at runtime without rebuilding:

```go
instance, err := app.RegisterAgent(ctx, "qa", agent.Definition{
    Card:   agent.AgentCard{Name: "Ticket QA"},
    Engine: agent.EngineRef{Kind: "agent.Engine", Impl: "graph"},
}, runtime.WithToolAssembly("shared_tools")) // optional; needs dynamic_catalog
if err != nil { /* Validation / Conflict / NotFound */ }

// Sessions work exactly like deployed agents:
lease, err := app.Sessions().GetOrCreate(ctx, session.Key{
    AgentID: "qa", ContextID: "user-7",
})

// Removal drains active turns (bounded) before closing engine/hooks:
if err := app.UnregisterAgent(ctx, "qa",
    runtime.WithRemoveTimeout(30*time.Second)); err != nil {
    /* DeadlineExceeded: registration restored, retryable */
}
```

## Session deletion (core/v0.1.27+)

`app.Sessions().DeleteSession(ctx, key)` removes one session's durable
state (committed history, parked-run checkpoint, resumable request) and
closes its live session — the by-key counterpart of `UnregisterAgent` for
delete/archive workflows. Semantics mirror removal: new opens for the key
are refused until deletion finishes, the live session is drained (bounded
by ctx), and on ctx expiry the delete marker rolls back with no partial
removal — retryable, idempotent, and a later `Open` starts with empty
history.

Rules:

- `RegisterAgent` uses the same assembly path as deployment
  (`deploy.BindAgent`); a name colliding with a deployed or registered
  agent is `Conflict`, assembly failures are `Validation`.
- `UnregisterAgent` blocks new sessions, waits for active turns (bounded
  by ctx or `WithRemoveTimeout`), then releases engine/hooks. Unknown
  names are an idempotent no-op; deployed agents cannot be removed at
  runtime (`Conflict`).
- With `dynamic_catalog` configured and no `default`, registration must
  carry `WithToolAssembly(<resource name>)`.
- `runtime.agent.<id>.registered` / `.removed` lifecycle events are
  published on success (subscribe with `PatternAgentLifecycle()`).

## Runtime reload (core/v0.1.14+)

`Runtime.Reload` transactionally replaces the deployment document without
rebuilding the runtime: the previous generation keeps serving until the
new one is built and swapped. Progress is published as
`runtime.rebuild.started` / `.completed` / `.failed` events
(`SubjectRuntimeRebuild*`, subscribe with `PatternRuntimeRebuild()`); a
failed reload aborts before any swap and the old generation stays in
service.

## Sources of truth

`core/runtime/config.go`, `core/runtime/lifecycle.go`,
`core/runtime/reload.go`, `core/runtime/events.go`,
`docs/guides/runtime.md`.
