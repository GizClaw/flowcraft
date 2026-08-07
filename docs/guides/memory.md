---
layout: default
title: Memory Stack
---
# Memory Stack Guide

FlowCraft's memory stack is split into three layers, mirroring the
inference stack: an SDK contract, a concrete implementation, and the
generic deploy/runtime glue.

| Layer                 | Owns                                                                      | Lives in          |
| --------------------- | ------------------------------------------------------------------------- | ----------------- |
| Contract              | `ContextProvider`, `TurnSink`, `DocumentSink`, `ContextRenderer`, `Scope` | `sdk/memory`      |
| Implementation        | canonical stores, projections, retrieval, worker, lifecycle maintenance   | `memory/`         |
| Glue                  | `memory.Assembly` deploy resource, `memory.context` / `memory.turn` hooks, GoTemplate renderer | `sdk/memory/config` + `sdkx/memory` |
| Runtime integration   | starting the implementation's background worker inside `sdkx/runtime`     | `memory/runtime`  |

`sdk/memory` deliberately knows nothing about agents, hooks, or storage
backends. It defines the capabilities a deployment binds to; the
`memory/` module is **one implementation** of those capabilities and owns
its own settings schema and background worker. Other implementations can
register under their own `impl:` name with their own parameters.

## Concepts

### Scope

`Scope` is the hard-partition key for every memory operation. Unlike the
older design, conversation and dataset addresses are **request fields**,
not scope fields; they can never widen the partition.

```go
type Scope struct {
    RuntimeID string // names the configured memory resource; required
    UserID    string // tenant partition; "" = explicit global scope
    AgentID   string // hard partition; "" = explicit global-agent partition
}
```

`Scope.Validate` requires `RuntimeID` and rejects NUL bytes. The effective
partition is `RuntimeID + UserID + AgentID` (`HardPartitionKey`); a request
with an empty `UserID` or `AgentID` is an intentional global scope, never a
widened read.

### ContextProvider

`ContextProvider` selects, hydrates, and packs memory for one turn. Retrieval
indexes and fusion strategies are implementation details.

```go
type ContextProvider interface {
    Context(ctx context.Context, request ContextRequest) (ContextResult, error)
}
```

`ContextRequest` carries the hard `Scope`, an optional `ConversationID`, a
query, `DatasetIDs`, a `Budget`, `MinScore`, and a stable `RecallEventID`.
The result is a packed `ContextResult` of `ContextItem` values with a token
count, truncation flag, and the same `RecallEventID`.

`ContextItem` is the hydrated memory view: kind (`raw_message`, `fact`,
`document_resource`, `document_section`, `document_chunk`,
`document_summary`, `summary`), content in `message.Content`, a score in
`[0,1]`, provenance `SourceRef`s, token count, source class (`recent`,
`long_term`, `summary`), and optional `ExpandHint` navigation metadata.

### TurnSink

`TurnSink` durably commits canonical conversation messages:

```go
type TurnSink interface {
    CommitTurn(ctx context.Context, turn Turn) error
}

type Turn struct {
    Scope          Scope
    ConversationID string
    IdempotencyKey string // agent.Identity.RunID in the per-run hook
    Messages       []message.Message
    Metadata       Metadata
}
```

`IdempotencyKey` is required; the canonical value is `agent.Identity.RunID`,
and the `memory.turn` hook stamps it for you. Success means the source and
its durable derivation work were accepted; asynchronous derivation failures
do not retroactively fail the commit.

### DocumentSink

`DocumentSink` stores normalized document content and provenance. URI
fetching, parsing, and media extraction happen before this boundary.

```go
type DocumentSink interface {
    PutDocument(ctx context.Context, document Document) error
}
```

A `Document` requires a hard `Scope`, `DatasetID`, `DocumentID`,
`IdempotencyKey`, non-empty `Content`, and at least one `SourceRef`.

### ContextRenderer

`ContextRenderer` projects a structured `ContextResult` into prompt content.
It is a consumer-side SPI: providers are responsible only for selecting,
hydrating, and packing items.

```go
type ContextRenderer interface {
    Render(ctx context.Context, result ContextResult) (message.Content, error)
}
```

`sdkx/memory/render` ships a GoTemplate renderer with a default template that
escapes recalled text as untrusted reference data. See
[Hooks](#hooks) for configuration.

## Building a runtime

There is no generic `memory.New` anymore. You build an implementation's
assembly through its own config package:

```go
import (
    flowcraftmemory "github.com/GizClaw/flowcraft/memory/config"
    sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
    "github.com/GizClaw/flowcraft/sdk/workspace"
    "time"
)

ws := workspace.NewMemWorkspace()
builder, err := flowcraftmemory.NewBuilder(ws, inferRuntime) // *inference.Runtime
if err != nil { /* ... */ }

assembly, err := builder.NewAssembly(ctx, flowcraftmemory.Settings{
    Generate: flowcraftmemory.ModelSettings{Provider: "openai", Name: "gpt-5.4"},
    Embed:    flowcraftmemory.ModelSettings{Provider: "openai", Name: "text-embedding-3-small"},
    Scopes:   []flowcraftmemory.ScopeSettings{{RuntimeID: "prod", UserID: "u1"}},
    Interval: flowcraftmemory.Duration(time.Hour),
})
if err != nil { /* ... */ }
defer assembly.Close()

// The assembly implements all three SDK capabilities:
var _ sdkmemory.ContextProvider = assembly.System
var _ sdkmemory.TurnSink        = assembly.System
var _ sdkmemory.DocumentSink    = assembly.System

// Run one derivation scan synchronously (the runtime integration runs this
// on a loop for you).
if err := assembly.Runner.RunOnce(ctx); err != nil { /* ... */ }
```

`assembly.System` exposes the canonical stores and retrieval provider;
`assembly.Runner` is the in-process derivation worker; `assembly.ChatDAG`,
`assembly.KnowledgeDAG`, `assembly.LifecycleDAG` describe the write-side
pipelines; `assembly.LifecycleRunner` owns durable maintenance ("Dreaming").

## Config: memory.yaml

The flowcraft implementation's settings document is strict and has **no
`version` field** — it is implementation-owned, not a versioned envelope.
`embed` is always required, and `generate` is required unless fact
extraction is disabled (`fact.strategy: none` with an empty `chat_dag`);
every other block has safe defaults.

```yaml
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

Model references are exact `inference.ModelRef` values (provider, name,
optional profile). Credentials and provider catalogs stay in `inference.yaml`
— `memory.yaml` never contains secrets.

Optional blocks, all strictly decoded:

| Block               | What it controls                                                                 |
| ------------------- | -------------------------------------------------------------------------------- |
| `scopes`            | scope seeds registered before worker scans; duplicates are rejected               |
| `interval`          | derivation scan interval (default `1m`)                                           |
| `projection`        | projection family name (default `default`)                                        |
| `fact`              | fact-extraction strategy and resource caps                                        |
| `chunk`             | knowledge chunk size, overlap, and deterministic hierarchy summaries              |
| `recent`            | recent-context caps (`max_items`, `max_tokens`)                                   |
| `summary`           | durable summary branch (compactor thresholds, depth, group size)                  |
| `bm25`              | BM25 algorithm version and `k1` / `b`                                             |
| `projection_storage`| LSM projection storage (`algorithm_version`, `max_segments`, `max_delta_bytes`)   |
| `reranker`          | optional programmatic post-fusion reranker (version required when enabled)        |
| `lanes`             | vector / bm25 / entity lane weights and score calibration                         |
| `chat_dag`          | conversation derivation nodes (default: one `extract-facts` node)                 |
| `knowledge_dag`     | document derivation nodes (default: one `chunk-document` node)                    |
| `lifecycle`         | durable maintenance: `disabled`, `periodic`, `interval`, `lease_ttl`, `owner`, `decay`, `forget` |
| `lifecycle_dag`     | lifecycle node order (default: integrate → compact → decay → forget → repair)     |

Custom algorithm/deriver/factory selections are programmatic-only and pass
through `FactoryCatalog`, `LifecycleFactoryCatalog`, and
`LifecycleEffects` on `Settings`.

## Deploy integration

`memory.Assembly` is a first-party deploy resource. The application registers
the implementation factory by name, mirroring inference provider factories:

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

The `sdk/memory` protocol itself hard-codes no dependencies: each
implementation declares the resource deps it needs at registration, and the
factory type-asserts them. The flowcraft implementation requires a workspace
(for the canonical stores) and an `inference.Runtime` (for generation and
embeddings), and exposes its system as the `system` item:

```yaml
resources:
  ws:
    kind: workspace.Registry
    impl: yaml
    settings: {file: ./workspace.yaml}
  infer:
    kind: inference.Assembly
    impl: yaml
    settings: {file: ./inference.yaml}
  memories:
    kind: memory.Assembly
    impl: flowcraft
    deps:
      workspace: ws/project
      inference: infer/runtime
    settings: {file: ./memory.yaml}
```

## Hooks

`sdkx/memory/hook` ships two lifecycle hooks; both bind the whole
`memory.Assembly` resource (not an item) as their `memory` dependency:

```yaml
agents:
  researcher:
    engine:
      kind: graph
      settings: {graph: ./graphs/researcher.json}
    deps:
      inference: infer
    prepare:
      - type: memory.context
        deps: {memory: memories}
        settings:
          query: {board: query}
          scope: {runtime_id: prod, user_id: u1}
          conversation_id: c-1
          budget: {max_items: 8, max_tokens: 1000}
          output: memory_items
          render:
            output: memory_prompt
            gotmpl: {}
    commit:
      - type: memory.turn
        deps: {memory: memories}
        settings:
          scope: {runtime_id: prod, user_id: u1}
          channel: __main_channel
```

`memory.context` settings:

- `query` is strict: set exactly one of `literal`, `board`,
  `current_message`, or `recent_only` (`board` reads a string from the Board
  passed by the previous preparer; `current_message` uses the turn's input
  text; `recent_only` skips long-term retrieval).
- `scope` carries `runtime_id`, optional `user_id`, optional `agent_id`.
- `conversation_id` defaults to `agent.Request.ContextID`.
- `dataset_ids` limits retrieval to named datasets.
- `budget` caps `max_items`, `max_tokens`, and `max_chars`.
- `output` names the Board var receiving `[]memory.ContextItem`.
- `render.output` names the Board var receiving rendered prompt content, and
  `render.gotmpl` selects the GoTemplate renderer (empty template = embedded
  default). `output` / `render.output` must be non-reserved vars and must
  differ from each other.

`memory.turn` settings:

- `scope` as above.
- `conversation_id` defaults to `agent.Request.ContextID`.
- `channel` names the Board channel to commit (default
  `agent.MainChannel` / `__main_channel`).

The turn hook stamps `IdempotencyKey = agent.Identity.RunID` and skips empty
channels.

## Runtime integration

Background derivation and lifecycle maintenance are owned by the
implementation. `memory/runtime` exposes a `sdkx/runtime` integration that
starts the flowcraft worker:

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
      deps: {memory: memories}
      settings: {}
```

`memory.worker` requires the `memory.Assembly` dependency, and its settings
must be empty — maintenance policy comes from `memory.yaml` (`lifecycle`,
`interval`, `scopes`). It starts the derivation runner and, unless lifecycle
is disabled, the durable dreaming runner; shutdown is reversed on Runtime
close.

## Error taxonomy

`memory.Error` is the typed error every capability returns. `errors.Is` /
`errors.As` walk the chain through `Unwrap`, and each kind maps onto an
`errdefs` classification:

| Kind                        | Meaning                                                |
| --------------------------- | ------------------------------------------------------ |
| `KindInvalidRequest`        | shape error (bad scope, empty turn, missing provenance) |
| `KindNotConfigured`         | capability called without an implementation            |
| `KindConflict`              | implementation reports a conflicting write              |
| `KindOperationInterrupted`  | context cancelled / deadline exceeded                   |
| `KindProviderFailure`       | storage or retrieval transport failure                  |
| `KindInternal`              | programming error (invalid result, ledger gap)          |

## Using the capabilities in Go

```go
// Commit a turn.
err := assembly.System.CommitTurn(ctx, sdkmemory.Turn{
    Scope:          sdkmemory.Scope{RuntimeID: "prod", UserID: "u1"},
    ConversationID: "c-42",
    IdempotencyKey: runID,
    Messages: []message.Message{
        message.NewTextMessage(message.RoleUser, "remember this"),
        message.NewTextMessage(message.RoleAssistant, "done"),
    },
})

// Recall context.
result, err := assembly.System.Context(ctx, sdkmemory.ContextRequest{
    Scope:          sdkmemory.Scope{RuntimeID: "prod", UserID: "u1"},
    ConversationID: "c-42",
    DatasetIDs:     []string{"knowledge"},
    Query:          "vector database",
    Budget:         sdkmemory.Budget{MaxItems: 8, MaxTokens: 1000},
    MinScore:       0.5,
    RecallEventID:  runID,
})

// Store a document.
err = assembly.System.PutDocument(ctx, sdkmemory.Document{
    Scope:          sdkmemory.Scope{RuntimeID: "prod", UserID: "u1"},
    DatasetID:      "knowledge",
    DocumentID:     "handbook",
    IdempotencyKey: "doc-run-1",
    Content:        message.Content{Parts: []message.Part{message.TextPart{Text: "# Handbook"}}},
    Provenance:     []sdkmemory.SourceRef{{Kind: sdkmemory.SourceDocument, ID: "source/handbook"}},
})
```

Leave storage-level identifiers to the implementation: the SDK contracts
require caller-supplied `IdempotencyKey` / `DocumentID` but never invent
stable IDs for you.

## Testing

There is no shared `memorytest` black-box suite in the current stack. The
contract is small enough to test directly:

- `sdk/memory` package tests assert the SPI invariants (scope validation,
  request/result validation, error classification).
- `memory/config/config_test.go` builds a real assembly over
  `workspace.NewMemWorkspace` + a fake inference runtime and asserts
  commit → derive → recall end to end.
- `sdkx/memory/hook/hook_test.go` covers both hook factories with a
  recording `Assembly`.
- `memory/runtime/integration_test.go` covers the runtime worker lifecycle.

For your own implementation, implement the three SPI methods and test the
same commit/recall/document round trip against your storage.

## Further reading

- Contract: `sdk/memory/doc.go`, `sdk/memory/assembly.go`,
  `sdk/memory/context.go`, `sdk/memory/turn.go`, `sdk/memory/document.go`,
  `sdk/memory/scope.go`, `sdk/memory/render.go`.
- Implementation: `memory/config/config.go` (Settings, defaults,
  validation), `memory/config/settings.go`, `memory/config/build.go`,
  `memory/runtime/integration.go`.
- Glue: `sdkx/memory/hook/hook.go`, `sdkx/memory/render/gotmpl.go`,
  `sdk/memory/config/resource.go`.
- Deploy wiring: [deploy.md](deploy.md) and
  [runtime.md](runtime.md) (the `memory.worker` integration).
