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
    dynamic_catalog:           # optional per-session dynamic tool catalog
      tools: {default: tool_asm, researcher: research_tools}
      default_exposure: deferred   # always|direct|deferred|hidden
      exposures: {tool_search: always}
      selected_retention: 5        # 0 uses the dynamic default (5)
      recent_window: 10            # 0 uses the dynamic default (10)
      budget: {max_definitions: 32, max_bytes: 16384}
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
  needs them passed via `WithExternalResourceConsumers`. Tool assemblies
  named by `sessions.dynamic_catalog.tools` are runtime references too.
- `sessions.resume: true` without `checkpoint_store` is an error.
- `sessions.dynamic_catalog.tools` is a required map from agent IDs to
  `tool.Assembly` resource names; the reserved `default` key is an
  optional fallback. Every referenced resource must exist and have kind
  `tool.Assembly`, every key must name a deployed agent (or `default`),
  and every deployed agent must have an entry unless `default` is set.
- `default_exposure` and `exposures` values must be one of
  `always`/`direct`/`deferred`/`hidden`; retention/window/budget fields
  must not be negative (zero means use the dynamic package default).
- Runtime creates one catalog per `(AgentID, ContextID)` session over
  the mapped assembly's shared registry, applies MCP source exposure
  metadata, and registers `tool_search`. Inference nodes that use the
  dynamic view should set `all_tools: true`.
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
