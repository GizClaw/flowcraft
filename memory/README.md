# FlowCraft Memory Implementation

This module is **one implementation** of the `sdk/memory` capability
(`ContextProvider`, `TurnSink`, `DocumentSink`). It owns canonical stores,
retrieval projections, the derivation worker, and durable lifecycle
maintenance. Other implementations can register under their own `impl:`
name; this README documents only the flowcraft implementation.

## Package map

| Package            | Responsibility                                                               |
| ------------------ | ---------------------------------------------------------------------------- |
| `storage/`         | canonical storage contracts: `Log`, `Store` (KV), errors, workspace adapters |
| `sources/message`  | canonical conversation messages (Log + commit metadata)                      |
| `sources/document` | canonical document revisions (Log events + KV current values)                |
| `views/`           | derived durable views: fact, observation, summary, document chunks           |
| `projection/`      | retrieval lanes (vector / BM25 / entity) over the LSM projection store       |
| `retrieval/`       | fusion, hydration, packing, and the `SearchBackend` contract + lane adapters |
| `worker/`          | derivation worker, checkpoints, watermarks                                   |
| `lifecycle/`       | durable maintenance DAG, outbox, decay, forget, repair                       |
| `config/`          | settings schema, driver registries, assembly builder                         |
| `runtime/`         | `sdkx/runtime` integration that starts the worker                            |

The architecture is described in
[docs/plans/2026-08-07-memory-storage-contract.md](../docs/plans/2026-08-07-memory-storage-contract.md)
(Log + KV canonical substrate, item-level `SearchBackend`, plugin-preserving
retrieval lanes).

## Building an assembly in Go

There is no `memory.New`. Construct the canonical backends and bind them:

```go
import (
    flowcraftmemory "github.com/GizClaw/flowcraft/memory/config"
    "github.com/GizClaw/flowcraft/memory/storage"
    "github.com/GizClaw/flowcraft/sdk/inference"
    "github.com/GizClaw/flowcraft/sdk/workspace"
    "time"
)

ws := workspace.NewMemWorkspace()
logStore, _ := storage.NewWorkspaceLog(ws)
kvStore, _ := storage.NewWorkspaceKV(ws)

builder, err := flowcraftmemory.NewBuilder(flowcraftmemory.Backends{
    Log: logStore,
    KV:  kvStore,
    // Search: map[string]retrieval.SearchBackend{...}, // optional per-lane injection
}, inferRuntime) // *inference.Runtime
if err != nil { /* ... */ }
builder.WithOutboxWorkspace(ws) // required only when lifecycle is enabled

assembly, err := builder.NewAssembly(ctx, flowcraftmemory.Settings{
    Generate: flowcraftmemory.ModelSettings{Provider: "openai", Name: "gpt-5.4"},
    Embed:    flowcraftmemory.ModelSettings{Provider: "openai", Name: "text-embedding-3-small"},
    Scopes:   []flowcraftmemory.ScopeSettings{{RuntimeID: "prod", UserID: "u1"}},
    Interval: flowcraftmemory.Duration(time.Hour),
})
if err != nil { /* ... */ }
defer assembly.Close()

// Run one derivation scan synchronously (the runtime integration runs this
// on a loop for you).
if err := assembly.Runner.RunOnce(ctx); err != nil { /* ... */ }
```

## Config: memory.yaml

The settings document is strict and has **no `version` field**. `embed` is
always required; `generate` is required unless fact extraction is disabled
(`fact.strategy: none` with an empty `chat_dag`); every other block has safe
defaults.

```yaml
storage:
  log: { driver: workspace }
  kv: { driver: workspace }
  search:
    lanes:
      vector: { driver: lsm }
      bm25: { driver: lsm }
      entity: { driver: lsm }
generate:
  provider: deepseek
  name: deepseek-v4-flash
embed:
  provider: openai
  name: text-embedding-3-small
scopes:
  - runtime_id: prod
    user_id: u1
interval: 1m
```

Model references are the flat configuration form of `inference.ModelRef`
(provider, name, optional profile). Credentials and provider catalogs stay in
`inference.yaml` — `memory.yaml` never contains secrets.

Optional blocks, all strictly decoded:

| Block                | What it controls                                                                                 |
| -------------------- | ------------------------------------------------------------------------------------------------ |
| `storage`            | canonical backends: `log` / `kv` drivers and `search.lanes` SearchBackend drivers                |
| `scopes`             | scope seeds registered before worker scans; duplicates are rejected                              |
| `interval`           | derivation scan interval (default `1m`)                                                          |
| `projection`         | projection family name (default `default`)                                                       |
| `fact`               | fact-extraction strategy and resource caps                                                       |
| `chunk`              | knowledge chunk size, overlap, and deterministic hierarchy summaries                             |
| `recent`             | recent-context caps (`max_items`, `max_tokens`)                                                  |
| `summary`            | durable summary branch (compactor thresholds, depth, group size)                                 |
| `bm25`               | BM25 algorithm version and `k1` / `b`                                                            |
| `projection_storage` | LSM projection storage (`algorithm_version`, `max_segments`, `max_delta_bytes`)                  |
| `reranker`           | optional programmatic post-fusion reranker (version required when enabled)                       |
| `lanes`              | vector / bm25 / entity lane weights and score calibration                                        |
| `chat_dag`           | conversation derivation nodes (default: one `extract-facts` node)                                |
| `knowledge_dag`      | document derivation nodes (default: one `chunk-document` node)                                   |
| `lifecycle`          | durable maintenance: `disabled`, `periodic`, `interval`, `lease_ttl`, `owner`, `decay`, `forget` |
| `lifecycle_dag`      | lifecycle node order (default: integrate → compact → decay → forget → repair)                    |

Custom algorithm/deriver/factory selections are programmatic-only and pass
through `FactoryCatalog`, `LifecycleFactoryCatalog`, and
`LifecycleEffects` on `Settings`.

## Storage backends

`storage.log` and `storage.kv` select drivers by name through
`config.DriverRegistry`:

- `workspace`: binds the resource's `workspace` dependency
  (`RegisterWorkspaceBackends`); the current default and the only built-in
  backend.
- SQLite / PG Log+KV drivers are planned (stage 6).

`storage.search.lanes` selects a `SearchBackend` driver per retrieval lane
(default `lsm`). The built-in `lsm` driver wraps each lane's
`component.Searcher` in a `retrieval.LaneBackend`. The read path keeps the
in-process algorithm plugin contract (`component.Registry` Searcher /
Indexer slots); OpenSearch can later replace individual lanes with its own
driver without touching fusion.

`Assembly.LaneBackends` exposes the resolved backends in canonical order
(vector, bm25, entity).

## Deploy integration

Register the flowcraft implementation with its deps:

```go
import (
    flowcraftmemory "github.com/GizClaw/flowcraft/memory/config"
    sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
    memoryconfig "github.com/GizClaw/flowcraft/sdk/memory/config"
)

deployBuilder.MustRegisterResource(
    memoryconfig.NewDeployFactory(
        "flowcraft",
        flowcraftmemory.Factory(),
        sdkconfig.ResourceDepSpec{Name: "inference", Type: "inference.Runtime", Required: true},
        sdkconfig.ResourceDepSpec{Name: "workspace", Type: "workspace.Workspace", Required: true},
    ),
)
```

```yaml
resources:
  ws:
    kind: workspace.Registry
    impl: yaml
    settings: { file: ./workspace.yaml }
  infer:
    kind: inference.Assembly
    impl: yaml
    settings: { file: ./inference.yaml }
  memories:
    kind: memory.Assembly
    impl: flowcraft
    deps:
      workspace: ws/project
      inference: infer/runtime
    settings: { file: ./memory.yaml }
```

`memory.yaml` must declare `storage.log` and `storage.kv` drivers; the
`workspace` driver consumes the `workspace` dep. `storage.search.lanes` is
optional and resolved after the lanes are built.

## Runtime integration

`memory/runtime` exposes a `sdkx/runtime` integration that starts the
derivation runner and, unless lifecycle is disabled, the durable dreaming
runner:

```go
import flowcraftruntime "github.com/GizClaw/flowcraft/memory/runtime"

if err := runtimeBuilder.RegisterIntegration(flowcraftruntime.NewFactory()); err != nil {
    return err
}
```

```yaml
runtime:
  event_bus: events
  scheduler: schedules
  integrations:
    - name: memory
      kind: memory.worker
      deps: { memory: memories }
      settings: {}
```

`memory.worker` requires the `memory.Assembly` dependency; its settings must
be empty — maintenance policy comes from `memory.yaml`.

## Assembly surface

- `System`: implements the three `sdk/memory` capabilities and exposes the
  canonical stores.
- `Runner`: in-process derivation worker; `RunOnce` for synchronous scans.
- `ChatDAG` / `KnowledgeDAG` / `LifecycleDAG`: write-side pipelines.
- `LifecycleRunner`: durable maintenance ("Dreaming").
- `LaneBackends`: `SearchBackend` per retrieval lane.

`Close` is idempotent: backends are borrowed and never closed by the
assembly; only the runners belong to it.

## Testing

- `make ci` runs vet + tests for every workspace module.
- `config/config_test.go` builds a real assembly over a memory workspace +
  fake inference runtime and asserts commit → derive → recall end to end.
- `storage/`, `sources/*`, `views/*`, `worker/`, `lifecycle/`,
  `retrieval/`, `projection/*` have behavior tests per package.
