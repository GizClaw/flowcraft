# Runtime section schema (`deploy.yaml` -> `runtime:`)

Owned by `core/runtime`. `deploy.Parse` preserves the subtree opaquely;
`runtime.DecodeConfig` decodes it strictly. `event_bus` is required when a
`runtime` section exists.

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

- `event_bus` must name an `event.Bus` resource.
- `checkpoint_store`, when present, must name an `agent.CheckpointStore`.
- `sessions.resume: true` requires `checkpoint_store`.
- `sessions.sink_buffer`, `delivery_concurrency`,
  `speculative_buffer_events`, and `speculative_buffer_bytes` are validated
  against hard upper bounds.
- `sessions.max_sessions` limits distinct live session keys.
- `dynamic_catalog.tools` maps agent IDs to `tool.Assembly` resources; the
  reserved `default` key is an optional fallback. Every deployed agent must
  be mapped directly or covered by `default`.
- Runtime references do not require an `export` flag in the core schema.

## Sources of truth

`core/runtime/config.go`, `core/runtime/doc.go`, `docs/guides/runtime.md`.
