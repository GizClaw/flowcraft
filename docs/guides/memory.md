---
layout: default
title: Memory Stack Guide
---
# Memory Stack Guide

FlowCraft's memory stack is split into three layers, mirroring the
inference stack: an SDK contract, concrete implementations, and the generic
deploy/runtime glue.

| Layer                 | Owns                                                                      | Lives in          |
| --------------------- | ------------------------------------------------------------------------- | ----------------- |
| Contract              | `ContextProvider`, `TurnSink`, `DocumentSink`, `ContextRenderer`, `Scope` | `sdk/memory`      |
| Implementation        | canonical stores, projections, retrieval, worker, lifecycle maintenance   | implementation modules (e.g. `memory/`) |
| Glue                  | `memory.Assembly` deploy resource, `memory.context` / `memory.turn` hooks, GoTemplate renderer | `sdk/memory` + `sdkx/memory` |

`sdk/memory` deliberately knows nothing about agents, hooks, or storage
backends. It defines the capabilities a deployment binds to. Implementations
own their settings schema and background workers, and register under their
own `impl:` name with their own parameters.

## Concepts

### Scope

`Scope` is the hard-partition key for every memory operation. Conversation
and dataset addresses are **request fields**, not scope fields; they can
never widen the partition.

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

## Deployment

`memory.Assembly` is a first-party deploy resource kind. There is no
generic `memory.New`: each implementation exposes its own
`config.Factory` and registers it under its `(kind, impl)` pair,
mirroring the graph engine:

```go
deployBuilder.MustRegisterResource(
    myMemory.Factory(), // implements config.Factory; Spec declares its deps
)
```

The built `memory.Assembly` is bound **whole** as hooks' `memory` dependency.
A complete deploy document with a concrete implementation is shown in the
[deploy guide](deploy.md).

## Hooks

`sdkx/memory/hook` ships two lifecycle hooks; both bind the whole
`memory.Assembly` resource (not an item) as their `memory` dependency:

```yaml
agents:
  researcher:
    engine:
      kind: graph
      settings: {graph: {file: ./graphs/researcher.json}}
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
implementation. An implementation may ship an `sdkx/runtime` integration that
starts its workers when Runtime starts; the flowcraft implementation's
`memory.worker` integration is described in the
[runtime guide](runtime.md).

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

Obtain an `sdkmemory.Assembly` from the deployment (for example as a bound
dependency), then use the three capabilities directly:

{% raw %}
```go
var assembly sdkmemory.Assembly // bound from the deployment

// Commit a turn.
err := assembly.CommitTurn(ctx, sdkmemory.Turn{
    Scope:          sdkmemory.Scope{RuntimeID: "prod", UserID: "u1"},
    ConversationID: "c-42",
    IdempotencyKey: runID,
    Messages: []message.Message{
        message.NewTextMessage(message.RoleUser, "remember this"),
        message.NewTextMessage(message.RoleAssistant, "done"),
    },
})

// Recall context.
result, err := assembly.Context(ctx, sdkmemory.ContextRequest{
    Scope:          sdkmemory.Scope{RuntimeID: "prod", UserID: "u1"},
    ConversationID: "c-42",
    DatasetIDs:     []string{"knowledge"},
    Query:          "vector database",
    Budget:         sdkmemory.Budget{MaxItems: 8, MaxTokens: 1000},
    MinScore:       0.5,
    RecallEventID:  runID,
})

// Store a document.
err = assembly.PutDocument(ctx, sdkmemory.Document{
    Scope:          sdkmemory.Scope{RuntimeID: "prod", UserID: "u1"},
    DatasetID:      "knowledge",
    DocumentID:     "handbook",
    IdempotencyKey: "doc-run-1",
    Content:        message.Content{Parts: []message.Part{message.TextPart{Text: "# Handbook"}}},
    Provenance:     []sdkmemory.SourceRef{{Kind: sdkmemory.SourceDocument, ID: "source/handbook"}},
})
```
{% endraw %}

Implementations may expose implementation-specific services (workers,
stores, backends) through their own types; the SDK contracts never require
them. Storage-level identifiers stay caller-supplied: the contracts require
`IdempotencyKey` / `DocumentID` but never invent stable IDs for you.

## Testing

There is no shared `memorytest` black-box suite; the contract is small enough
to test directly:

- `sdk/memory` package tests assert the SPI invariants (scope validation,
  request/result validation, error classification).
- `sdkx/memory/hook/hook_test.go` covers both hook factories with a
  recording `Assembly`.

For your own implementation, implement the three SPI methods and test the
same commit/recall/document round trip against your storage.

## Further reading

- Contract: `sdk/memory/doc.go`, `sdk/memory/assembly.go`,
  `sdk/memory/context.go`, `sdk/memory/turn.go`, `sdk/memory/document.go`,
  `sdk/memory/scope.go`, `sdk/memory/render.go`.
- Glue: `sdkx/memory/hook/hook.go`, `sdkx/memory/render/gotmpl.go`,
  `sdk/memory/assembly.go` and the implementation module.
- Deploy wiring: [deploy.md](deploy.md) and [runtime.md](runtime.md).
