---
layout: default
title: Memory Stack
---
# Memory Stack Guide

`sdk/memory` is FlowCraft's typed kernel for agent memory: six root
operations, a hard-partition `Scope`, a compile-enforced ledger, and
two wire shapes (`Record` for transcripts, `Hit` for retrieval). The
kernel knows nothing about agents or hooks — every deployment
decision is layered on top by `sdkx/memory/*`.

The stack is intentionally split:

| Layer                | Owns                                                                | Lives in                                |
| -------------------- | ------------------------------------------------------------------- | --------------------------------------- |
| Kernel               | six ops, scope, record, ledger, error taxonomy                      | `sdk/memory`                            |
| Config / assembly    | `memory.yaml` schema, `Builder`, slot routing, inference wiring     | `memory/config`                    |
| Lifecycle hooks      | `Load` / `Recall` as Preparer, `Append` as Committer                | `sdkx/memory/hook`                      |
| Tool                 | `Import` as `*tool.Tool` (host-registered)                           | `sdkx/tool/memory`                      |
| Background tasks     | cron-driven `Compact` / `Archive` through the generic scheduler     | `sdkx/scheduler/memory`                 |
| Test contract        | `RunAppend` / `RunLoad` / ...                                       | `sdk/memory/memorytest`                 |

The kernel is the only place the contract lives. The `sdkx` layers
add nothing of substance — they pick which lifecycle surface each
op attaches to and own the YAML ↔ Go plumbing.

## Concepts

### The six root ops

| Op          | Wire shape                            | Integration surface            |
| ----------- | ------------------------------------- | ------------------------------ |
| `Append`    | `[]Record` → `LastSeq`                | `agent.Committer`              |
| `Load`      | `Cursor` → `[]Record` + `NextCursor`  | `agent.Preparer`               |
| `Recall`    | `Query` → `[]Hit`                     | `agent.Preparer`               |
| `Import`    | `Source` → `DocumentID` + `ChunkCount`| `*tool.Tool` (host-registered) |
| `Compact`   | `OlderThan` → `Compacted` / `Bytes`   | background scheduler           |
| `Archive`   | `OlderThan` → `Archived` / `Bytes`    | background scheduler           |

Every op is narrow by design: typed `Request` / `Response`, a
`Compile` ledger, and a `FieldID` per canonical field. The kernel
rejects a request whose Compile ledger is incomplete or has a
`Rejected` decision.

### Scope

`Scope` is the addressing key for every op. Two hard-partition
fields — `RuntimeID` and `UserID` — fix the physical record an op
must touch; soft fields (`AgentID`, `ConversationID`, `DatasetID`)
are hints the impl may use for filtering or bucketing without ever
widening the hard partition.

```go
type Scope struct {
    RuntimeID      string  // hard partition, required
    UserID         string  // hard partition, "" = global
    AgentID        string  // soft filter
    ConversationID string  // soft filter
    DatasetID      string  // soft filter
}
```

The kernel applies `Spec.DefaultScope` only when the request carries a
completely zero-value `Scope`, then rejects a scope whose `RuntimeID` is empty
or does not match `Spec.RuntimeID`. A scope with an explicit `RuntimeID` and an
empty `UserID` is an intentional global scope; the default tenant is not
filled into it.

### Record and Hit

```go
type Record struct {
    ID      string             // stable caller id; runtime back-fills when empty
    Seq     uint64             // runtime-assigned monotonic seq
    Message inference.Message  // canonical content, reused from sdk/inference
}

type Hit struct {
    ID       string            // stable across recalls
    Parts    []inference.Part  // canonical parts
    Score    float64           // impl-native relevance
    Source   string            // opaque locator (e.g. "transcript:abc/seq-42")
    Metadata map[string]string // advisory annotations
}
```

`Record` is the transcript shape: stable ID, monotonic Seq,
canonical message. `Hit` is the retrieval shape: same ID
namespace, parts in inference's wire format, plus impl-native
score and a free-form `Source` callers can prefix-match on
("transcript:…", "chunk:…").

### IdempotencyKey

`Append` is the only mutating op in the per-Run path. It carries
an `IdempotencyKey` so a `Committer` retrying after an ambiguous
transport failure re-runs with the same key and the runtime
dedups the second write against the first. The canonical value
is `agent.Identity.RunID`; the `memory.append` hook
(`sdkx/memory/hook`) sets it for you.

### Cursor

`Load` returns `NextCursor` when more records remain; pass it
back as `Cursor` on the next call. The cursor format is opaque
to the kernel — impls write what makes sense for their storage
("page=4", "after-seq=42"), and the agent treats it as a
pass-through string.

## The kernel

### Building a Runtime

```go
import "github.com/GizClaw/flowcraft/sdk/memory"

rt, err := memory.New(memory.Spec{
    RuntimeID:    "prod",
    DefaultScope: memory.Scope{RuntimeID: "prod", UserID: "u-default"},
    Clock:        memory.SystemClock,
}, memory.Impls{
    Append:  myAppender,   // implements AppendOp (Compile + Execute)
    Load:    myLoader,     // implements LoadOp
    Recall:  myRecaller,   // implements RecallOp
    // Import, Compact, Archive are optional; nil = KindNotConfigured
})
```

`memory.New` validates `Spec.RuntimeID` and constructs the
runtime. `Impls` are copied by value; mutating the caller's
struct later does not affect the runtime.

Each public `Runtime.CompileXxx` result is the authoritative execution
ledger: the runtime applies scope and limit defaults, applies kernel policy,
then calls the configured implementation's `CompileXxx`. Implementation
rejections therefore appear directly in the public result. A missing
implementation returns a complete ledger of `ReasonNotConfigured`
rejections. `ExecuteXxx` reuses that same composition path and compiles the
effective request exactly once.

### NoopRuntime for tests

`memory.NewNoopRuntime` returns a `Runtime` whose every op is
satisfied by a no-op implementation. Use it when an agent is
deployed without a memory backend, in tests that need a
`Runtime` value but no real storage, and as the default
fallback when a deploy document references a memory hook
without binding a `memory.Assembly` resource.

```go
rt, _ := memory.NewNoopRuntime(memory.Spec{RuntimeID: "test"})
```

The noop ops emit one `Native` decision per canonical field, so
the kernel's ledger check always passes; `Execute` returns the
zero response.

### Error taxonomy

`memory.Error` is the typed error every op returns. The `Kind`
is stable; `errors.Is` / `errors.As` walk the chain through
`Unwrap` to the underlying cause, and the errdefs predicates
(`errdefs.IsValidation`, `errdefs.IsNotAvailable`, …) recognise
the classified wrapper.

| Kind                  | Meaning                                                |
| --------------------- | ------------------------------------------------------ |
| `KindInvalidRequest`  | shape error (empty Records, malformed Scope, …)        |
| `KindScopeInvalid`    | `Scope.Validate` failed                                |
| `KindNotConfigured`   | op called without a registered impl                    |
| `KindPolicyDenied`    | hook settings forbid the op                            |
| `KindProviderFailure` | impl-side transport or persistence failure             |
| `KindInternal`        | programming error (ledger gap, …)                      |

## Config: memory.yaml

The kernel is wire-shape only; the deploy-side `memory.yaml` is
where you set scope defaults, lifecycle intervals, embedding
config, and which `StoreFactory` backs each slot. The shape
lives in `memory/config/settings.go`; the YAML deploy factory
that wires it into a `Builder` is `sdk/memory/config`.

Minimal example:

```yaml
version: v1
runtime:
  hard_partition: [runtime_id, user_id]
  default_scope:
    runtime_id: prod
  clock:
    impl: system
stores:
  messages:
    impl: noop
```

Two logical slots, two roles:

| Slot          | Owns the op(s)            | Typical use                  |
| ------------- | ------------------------- | ---------------------------- |
| `messages`    | `Append` / `Load` / `Recall` | transcript persistence    |
| `documents`   | `Import` / `Compact` / `Archive` | knowledge ingestion   |
The deploy factory in `sdk/memory/config` exposes
`memory.Assembly` as a single `ResourceFactory` (`kind:
memory.Assembly`, `impl: yaml`); the host reads the per-slot
implementation name (`noop` in the example above) and dispatches
to the registered `StoreFactory`.

Embedding is an optional policy on the `documents` store, not a third store
slot. The completely empty block disables it. When enabled, all fields are
required and the memory resource must depend on an inference runtime:

```yaml
embedding:
  model:
    id:
      provider: openai
      name: text-embedding-3-small
    profile: default
  dimensions: 1536
  batch_size: 32
  timeout: 30s
```

`dimensions` is sent in `inference.EmbedRequest` and is the expected vector
schema, `batch_size` controls import batching, and `timeout` bounds each
inference call. Provider credentials, endpoints, and model catalogs belong in
`inference.yaml`, never `memory.yaml`.

The three files connect as follows:

```yaml
# deploy.yaml
version: v1
resources:
  infer:
    kind: inference.Assembly
    impl: yaml
    settings: {file: ./inference.yaml}
  memory:
    kind: memory.Assembly
    impl: yaml
    deps:
      inference: infer/runtime
    settings: {file: ./memory.yaml}
agents:
  researcher:
    engine: {kind: graph, settings: {graph: ./graphs/researcher.json}}
    deps:
      inference: infer
    prepare:
      - type: memory.load
        deps: {runtime: memory/runtime}
        settings: {into: transcript, limit: 50}
      - type: memory.recall
        deps: {runtime: memory/runtime}
        settings:
          into: hits
          query: {board: query}
          top_k: 8
    commit:
      - type: memory.append
        deps: {runtime: memory/runtime}
        settings: {channel: __main_channel}
```

```yaml
# inference.yaml
version: v1
providers:
  - id: openai
    driver: openai
    profiles:
      - id: default
        operations: [embed]
        secrets:
          api_key: {resolver: env, key: OPENAI_API_KEY}
```

```yaml
# memory.yaml
version: v1
runtime:
  hard_partition: [runtime_id, user_id]
  default_scope: {runtime_id: prod}
  clock: {impl: system}
stores:
  messages: {impl: sqlite, settings: {file: ./memory/messages.db}}
  documents: {impl: sqlite, settings: {file: ./memory/documents.db}}
embedding:
  model:
    id: {provider: openai, name: text-embedding-3-small}
    profile: default
  dimensions: 1536
  batch_size: 32
  timeout: 30s
```

The memory resource receives only `infer/runtime`. Hooks keep their own
`deps` entry on every hook item and continue to bind `memory/runtime`.

## Integration surfaces

Each op maps to one of four agent-facing surfaces. The
mapping is a deploy-time decision, not a kernel contract — the
kernel sees `AppendRequest`, the agent sees whatever interface
the deploy layer wires.

| Op          | Surface          | Lives in               |
| ----------- | ---------------- | ---------------------- |
| `Append`    | `agent.Committer`| `sdkx/memory/hook`     |
| `Load`      | `agent.Preparer` | `sdkx/memory/hook`     |
| `Recall`    | `agent.Preparer` | `sdkx/memory/hook`     |
| `Import`    | `*tool.Tool`     | `sdkx/tool/memory`     |
| `Compact`   | scheduler        | `sdkx/scheduler/memory`|
| `Archive`   | scheduler        | `sdkx/scheduler/memory`|

### Preparer / Committer: load, recall, append

The three per-Run ops attach to the agent lifecycle. The
`memory.load` and `memory.recall` factories return a
`Preparer`; `memory.append` returns a `Committer`. Each
factory reads typed settings off the deploy document and
resolves the `*memory.Runtime` from the `runtime:` dep the
`memory.Assembly` resource exposes.

```yaml
agents:
  researcher:
    engine: { kind: graph }
    prepare:
      - type: memory.load
        deps:
          runtime: memory/runtime
        settings:
          into: transcript
          limit: 50
          conversation: c-1
      - type: memory.recall
        deps:
          runtime: memory/runtime
        settings:
          into: hits
          query:
            board: query
          top_k: 8
    commit:
      - type: memory.append
        deps:
          runtime: memory/runtime
        settings:
          channel: __main_channel
          conversation: c-1
```

- `load` writes the records as `inference.Message` into the
  named channel; the engine reads the channel as a normal
  transcript.
- `recall` writes the hits into the named board var as
  `[]memory.Hit`; the prompt builder reads it.
- `recall.settings.query` is a strict `QuerySpec`: set exactly one of
  `literal: <text>` or `board: <var-name>`. A board query reads the string from
  the Board passed by the preceding lifecycle stage; missing and non-string
  values are validation errors. `Request.Inputs` are copied into Board vars by
  the default seeder before preparers run, so `board: query` can read a request
  input and can also read a value written by an earlier preparer. No
  `{{inputs.*}}` or `{{board.*}}` template parsing is performed.
- `append` reads the named channel at commit time, sends the
  messages as records, and stamps `IdempotencyKey =
  agent.Identity.RunID`.
- When `settings.conversation` is omitted, all three hooks fall back
  to `agent.Request.ContextID`. An empty `ContextID` disables
  transcript load/append for that turn; recall keeps an empty
  conversation as an intentional global search.

The `Scope` block in any of these settings is optional; empty
fields fall back to `Spec.DefaultScope` so most deployments
omit it.

### Tool: import

`memory.import` is a `*tool.Tool` the host registers into a
`tool.Registry` before the deploy document runs. The deploy
side only references the tool by name in the agent allow-list.

```go
import (
    memtool "github.com/GizClaw/flowcraft/sdkx/tool/memory"
    "github.com/GizClaw/flowcraft/sdk/tool"
)

// At host boot, with the assembly already built:
reg := tool.NewRegistry()
memtool.RegisterImportTool(reg, assembly.Runtime, memtool.ImportSettings{
    Scope:     memtool.ScopeConfig{RuntimeID: "prod"},
    DatasetID: "knowledge",
    Source:    "/docs/handbook.md", // optional when every call supplies source
})

deployBuilder.RegisterSource("host.tools", func(_ context.Context, _ string) (any, error) {
    return reg, nil
})
```

```yaml
agents:
  researcher:
    engine: { kind: graph }
    deps:
      tools: { source: host.tools }
    tools: [memory.import]
```

The LLM sees a `memory.import` tool with a typed
`InputSchema`; calls land in `Runtime.ExecuteImport` and
return `DocumentID` / `ChunkCount` as JSON.
Tool arguments must be one JSON object: `null`, arrays, unknown fields,
and trailing JSON values are rejected. `Source` may be omitted from
`ImportSettings`, but the merged configured/per-call request must contain a
non-empty source before execution.

### Scheduler: compact, archive

`Compact` and `Archive` are wall-clock maintenance, not
per-Run work. The kernel does not schedule them; a host-owned
scheduler does.

```yaml
lifecycle:
  compact:
    cron: "@hourly"
    older_than: 168h
    keep: 50
  archive:
    cron: "0 3 * * *"
    older_than: 2160h
    destination: s3://bucket/archive
```

```go
import localscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"
import memoryscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler/memory"

server, err := localscheduler.NewLocalServer()
if err != nil { /* ... */ }
sched, err := memoryscheduler.New(
    ctx,
    server,
    "memory",
    assembly.Runtime,
    assembly.Lifecycle,
)
if err != nil { /* ... */ }
if err := sched.Start(); err != nil { /* ... */ }
if err := server.Start(); err != nil { /* ... */ }
defer server.Close()
defer sched.Close()
```

Each empty operation block is disabled. An enabled block requires both
`cron` and a positive `older_than`; archive also requires `destination`.
The adapter creates one `sdk/scheduler.Registration` on the shared Server. It
registers stable compact/archive rules using `OverlapSkip` and starts a leased
worker for its namespace. The Server only stores wire tasks and execution
leases; it never receives a Go callback. This allows the same registration to
use a local or remote Server.

At trigger time the worker computes the cutoff from the runtime clock and
executes the operation synchronously. Compact and Archive use worker
concurrency one, so they cannot mutate the same storage concurrently.
Execution errors become failed scheduler executions; a later cron trigger can
retry. `Close` cancels in-flight memory I/O and stops only the worker; the
application owns and closes the shared Server.

## Testing

### Shared contract

`sdk/memory/memorytest` ships black-box conformance suites
(`RunAppend`, `RunLoad`, `RunScope`, `RunNoop`, …). An impl
that wants to prove it conforms wires a `*memory.Runtime` and
calls them:

```go
func TestConformance(t *testing.T) {
    memorytest.RunScope(t, memorytest.ScopeSuite{
        Spec:       memory.Spec{RuntimeID: "test"},
        BuildRuntime: func(t *testing.T) *memory.Runtime {
            rt, _ := myImpl.Build(t)
            return rt
        },
    })
    memorytest.RunAppend(t, memorytest.AppendSuite{
        Spec:         memory.Spec{RuntimeID: "test"},
        BuildRuntime: myImpl.Build,
        SampleScope:  memory.Scope{RuntimeID: "test", UserID: "u1"},
    })
}
```

The suites assert the documented properties: `LastSeq`
monotonicity, `IdempotencyKey` dedup, cursor pagination,
opaque `Source` / `NextCursor`, scope partitioning. They do
not assert relevance quality on `Recall` — that is an
impl-level concern.

### Noop reference

`memorytest.RunNoop` runs the full contract against
`memory.NewNoopRuntime`. If `RunNoop` fails the bug is in the
kernel, not in the individual impls.

### Per-package tests

- `sdk/memory` — kernel-level tests (Compile invariants, scope
  validation, ledger enforcement, NotConfigured rejection).
- `sdkx/memory/hook` — factory-level tests (build rejects, end-to-end
  with a recording runtime, `IdempotencyKey` passthrough, error
  surface).
- `sdkx/scheduler/memory` — adapter-level tests (task mapping,
  cron registration, cross-rule serialization, close cancellation).
- `sdkx/tool/memory` — tool-level tests (definition schema,
  `Execute` overrides, error surface, registry wiring).

## Common patterns

### Per-tenant transcripts

`Scope.UserID` is the tenant boundary. A single `Runtime` can
back many tenants; the kernel guarantees two requests with
different `UserID` never touch the same record.

```go
memory.New(memory.Spec{
    RuntimeID: "prod",
    DefaultScope: memory.Scope{RuntimeID: "prod"},  // no UserID
}, impls)
```

A request with `UserID = ""` lands in the global scope,
which is documented behaviour, not the zero value. Set
`Spec.DefaultScope.UserID` if you want to forbid global
scopes.

### Appending with idempotency

```go
rt.ExecuteAppend(ctx, memory.AppendRequest{
    Scope:          memory.Scope{RuntimeID: "prod", UserID: "u1"},
    ConversationID: "c-42",
    IdempotencyKey: id.RunID,         // see sdk/agent.Identity
    Records: []memory.Record{
        {Message: userMsg},
        {Message: assistantMsg},
    },
})
```

Leave `Record.ID` empty: the runtime back-fills it with a
unique value before persisting. `Record.Seq` is also
runtime-assigned; callers leave it zero.

### Recall with a soft filter

```go
rt.ExecuteRecall(ctx, memory.RecallRequest{
    Scope:    memory.Scope{RuntimeID: "prod", UserID: "u1"},
    Query:    "vector database",
    TopK:     8,
    MinScore: 0.5,
    Filters:  map[string]string{"dataset": "knowledge"},
})
```

`Filters` is opaque to the kernel. Implementations apply it
during scoring; the kernel just makes sure the field is part
of the active-field set and the ledger declares it `Native`.

## Further reading

- Package contracts: `sdk/memory/doc.go`, `sdk/memory/runtime.go`,
  `sdk/memory/types.go`, `sdk/memory/error.go`.
- Test suites: `sdk/memory/memorytest/doc.go`.
- Deploy-side wiring: `memory/config/doc.go`,
  `sdk/memory/config/doc.go`.
- Lifecycle hooks: `sdkx/memory/hook/doc.go`.
- Tool integration: `sdkx/tool/memory/doc.go`.
- Background tasks: `sdkx/scheduler/memory/scheduler.go`.
- Lifecycle contract: `sdk/agent/doc.go` (`Preparer`,
  `Committer`).
- Sibling guide: [deploy.md](deploy.md) (resource and hook
  registration; the `memory.Assembly` resource is the entry
  point).
