# Runtime section schema (`deploy.yaml` → `runtime:`)

Owned by `sdkx/runtime`. `deploy.Parse` preserves the subtree opaquely;
`runtime.DecodeConfig` decodes it strictly. When the section is present,
`event_bus` is required.

```yaml
runtime:
  event_bus: events            # required resource name
  scheduler: schedules         # optional resource name
  checkpoint_store: checkpoints # optional resource name
  sessions:
    idle_timeout: 10m          # default 10m, positive
    sink_buffer: 256           # default 256, positive
    speculative_buffer_events: 1024   # default 1024, positive
    speculative_buffer_bytes: 1048576 # default 1 MiB, positive
    resume: false              # true requires checkpoint_store
  integrations:
    - name: delegation         # unique, required
      kind: delegation.local   # required
      deps:
        backend: delegations   # whole-resource names only (no "/")
      settings: {max_concurrency: 4, max_depth: 8, timeout: "30s"}
```

## Rules

- `event_bus`, `scheduler`, `checkpoint_store`, and every integration dep
  are exact resource map keys; runtime never discovers resources by kind.
- Runtime references are external consumers during deploy assembly:
  those resources do not need `export: true`, but plain `deploy.Builder`
  needs them passed via `WithExternalResourceConsumers`.
- `sessions.resume: true` without `checkpoint_store` is an error.
- `checkpoint_store` must name an `agent.CheckpointStore` resource
  (sqlite or workspace-backed).
- Durations are Go duration strings (`10m`, `30s`); unitless numbers are
  rejected.
- Integrations: unique names, non-empty kind, whole-resource deps only,
  settings strictly decoded by the integration factory.
- `delegation.local` accepts `max_concurrency`, `max_depth`, `timeout`;
  its `backend` dep is optional.
- Other integration kinds (e.g. the flowcraft memory worker) are
  app-registered and ship with their implementation module; the flowcraft
  `memory/` module is not released yet, so examples omit it.

## Sources of truth

`sdkx/runtime/config.go`, `sdkx/runtime/doc.go`, `docs/guides/runtime.md`.
