---
layout: default
title: Graph Engine
---
# Graph Engine Guide

`core/agent-graph` is an `agent.Engine`: an agent as a graph of nodes driven by a
frontier. It is a sibling of `core/agent`, not a layer above it — `agent` defines
what a task and its events are, `graph` runs nodes whose edges decide where the
run goes next, and both produce `agent.Event` streams, so a deployment exposes
either through the same protocol.

## The run is a frontier loop, not a wave over a DAG

A graph may loop: a ReAct cycle is model → tools → model. There is therefore no
topological order and no cycle rejection. Each step:

1. invokes the frontier — the nodes reached in the previous step;
2. applies the step's writes, in declaration order;
3. evaluates the outgoing edges of every node that ran, to compute the next
   frontier.

An edge fires when its condition holds (an empty condition always fires), so a
node fans out by having several outgoing edges, and branching joins implicitly: a
node reached twice in one step runs once. `limits.max_steps` bounds a loop that
never empties the frontier.

## No board, and no values on edges

The previous engine kept a board of untyped vars copied per branch and merged
with reflection. Three things followed, and this package is built to avoid all
three:

- **An edge carries a condition, not a value.** A wrong edge used to surface as a
  runtime type assertion with no context; here routing is a compiled expression
  over the state, so a bad one fails at build.
- **A fan-out shares immutable values.** A message list is a persistent sequence:
  appending is O(1) and shares its parent, so a branch costs its own work, not the
  size of the history.
- **Merging is declared, not diffed.** A state key declares its merge rule, and a
  rule that cannot decide is an error rather than a silent winner.

A node produces state and nothing else. Values do not travel along edges: in a
loop, "the value on this edge" has no answer, so the state is the only carrier
that stays well defined.

## The document

The graph is its own file; the deployment's engine settings only name it:

```yaml
# deploy.yaml
agents:
  - name: forge
    engine:
      kind: graph
      settings:
        path: ./graphs/react.yaml
    runtime:
      layers: [{priority: 0, path: ./runtime.yaml}]
```

```yaml
# graphs/react.yaml
version: 1
name: react
entry: model
input: messages

limits:
  max_steps: 24
  max_nodes: 200
  timeout: 5m
  concurrency: 4

checkpoint:
  every: 8              # also write a base every N steps

stream:
  state: false          # report each step's writes as a GraphState event

state:
  messages: {type: messages, merge: append, carry: true}
  pending: {type: json}
  answer: {type: text}

nodes:
  - id: model
    kind: model
    config:
      messages: messages
      pending: pending
      usage: usage
      request:
        model: {provider: openai, name: gpt-5}
        system: "You are a coding agent."
        tools: {names: [read, write, exec]}
  - id: tools
    kind: tool
    config:
      messages: messages
      pending: pending
      parallelism: 4
  - id: done
    kind: condition
    config:
      key: answer

edges:
  - {from: model, to: tools, when: "len(pending) > 0"}
  - {from: model, to: done, when: "len(pending) == 0"}
  - {from: tools, to: model}

output:
  key: answer
```

| Field | Rule |
| --- | --- |
| `version` | Must be `1`. |
| `entry` | The node the first frontier holds. |
| `input` | The state key `Start` seeds with the run's messages. Empty means the graph takes no input, and a caller that supplies some is refused. |
| `limits` | `max_steps`, `max_nodes` (skipped nodes count: a cycle of skipped nodes still spends budget), `timeout`, `concurrency`. Zero means no cap — the caller's context is always the outer bound. |
| `checkpoint.every` | `0` writes a base only when the deltas would be missing; a positive value also writes one every N steps, bounding how long a restart replays. |
| `stream.state` | Emit a `GraphState` event per step. Off by default: it costs a JSON projection per step, and a run nobody is watching should not pay for one. |
| `output.key` | The state key a finished run reports. |

YAML is the written form and JSON is accepted; both go through one strict
decoding, so an unknown field is an error either way.

## State

| Field | Values |
| --- | --- |
| `type` | `text`, `json`, `messages`, `bool`. |
| `merge` | `single` (default: a second write in one step is an error), `last` (last write wins), `append` (the writes concatenate). |
| `carry` | Keep this key across the tasks of one session: a new task seeds it from the previous task's state instead of the declared zero value. |

`carry` is the document's way to say "this is the conversation, not the
request". It is meant for bounded state — a game, a form, a plan — which carries
in microseconds. A key that grows without bound costs its own size on every turn,
because the state is re-read from the store: a 200-message transcript measured
667 µs and 5000 allocations per turn. A long conversation belongs in one task,
not in a carried key.

Nodes in one step read the state as it was when the step started. Their writes
apply after the step, in declaration order — which is why `merge: single` turns
"two nodes wrote the same key" into an error instead of a coin flip.

## Nodes

```yaml
- id: model                 # unique within the document
  kind: model               # a registered kind; unknown is a load error
  config: { ... }           # the kind decodes this strictly
  skip: "step > 10"         # a condition: when it holds, the node is not invoked
  timeout: 90s              # one invocation
  retries: 2                # retryable failures
```

A skipped node is not invoked, **but its outgoing edges still fire**: it routes
without running. That is what makes `skip` useful for a terminating branch.

### `model`

One model call per invocation.

| Config | Meaning |
| --- | --- |
| `messages` | State key carrying the conversation (a `messages` value). |
| `pending` | State key the call's tool calls are written to. |
| `usage` | State key the token usage is appended to. |
| `output` | State key the response text is written to. |
| `request.stream` | Nil streams (incremental events, what a reader watches); `false` returns one finished response for a call whose answer is data. |
| `request.model` | `provider`, `name`, `profile` — resolved against the runtime's routing table. |
| `request.system` | The system framing for this call. |
| `request.tools` | `enabled`, `names` (restrict the offer), `require` (force visible in a dynamic catalog), `choice`. |
| `request.extensions` | Provider extensions, decoded by the registered `ExtensionDecoder`s. |

### `tool`

Executes the pending tool calls.

| Config | Meaning |
| --- | --- |
| `messages`, `pending` | As above: read the conversation, drain the pending calls. |
| `effects` | `after` (default) records a finished call; `before` writes the started record before the tool runs, so a crash leaves an outcome to reconcile instead of a silent duplicate. |
| `parallelism` | Caps how many calls of one batch run at once; `0` leaves the batch unbounded. |

### `condition`

Evaluates an expression and writes the branch it took.

```yaml
- id: done
  kind: condition
  config: {key: answer, when: "answer != nil", then: yes, else: no}
```

### `script`

Not built in: a host registers it, because a script engine is a dependency
choice. See [script.md](script.md).

## Conditions

Conditions are [expr](https://expr-lang.org) expressions over the state as it was
when the step started, plus `step`:

```yaml
skip: "step > 10"
when: "pending != nil and len(pending) > 0"
```

They are compiled once at build, so a typo is a load error rather than a
run-time surprise, and a step evaluates its skip conditions and its edges against
one snapshot. Values keep their shape in the environment: text is a string, a
number or object is decoded JSON, a message list is the slice (so `len(messages)`
works), media is its descriptor. A condition that returns a non-bool is a run
error naming the node.

## Extending it with a node kind

```go
type NodeKind interface {
    Name() string
    Invoke(ctx context.Context, in Invoke) (NodeStream, error)
}

// Optional: parse the config once, at Build.
type ConfigDecoder interface {
    DecodeConfig(raw json.RawMessage) (any, error)
}
```

`Invoke` carries everything a node needs and nothing it could misuse:

| Field | Use |
| --- | --- |
| `Node`, `Config`, `Decoded` | The instance id, its raw settings, and the value `Build` decoded. |
| `State(key)`, `Snapshot()` | Read a declared key, or the whole step. The snapshot is read-only: it is shared by every node of the step. |
| `Step` | The loop counter, so a node can decide to stop. |
| `Answer` | The reply to a question this node asked by suspending. |
| `Lookup`, `Replay` | A recorded model call by request hash, and whether the run must answer from records (a miss is an error, not a silent live call). |
| `Records(kind)`, `Checkpoint` | Durable facts earlier steps recorded, and an immediate write for a side effect that must be recorded before it runs. |
| `Trace`, `Bodies` | How much is traced, and whether response bytes are kept. |

A node returns a `NodeStream` rather than calling back into the engine — the
engine pulls, so backpressure and cancellation are the contract rather than a
convention, and a kind can be tested by feeding it inputs and reading the stream.
Most kinds compute and return `Static(result)`; only one that reports progress (a
streaming model call, a long tool run) needs its own stream.

`Result` has three fields: `State` (the writes), `Records` (durable facts),
`Suspend` (a question). A node that suspends loses its writes — it is re-entered
from the start on resume and produces them again, so committing them now would
apply them twice.

Register kinds where the graph is opened:

```go
built, err := nodes.Open(ctx, rt, "graphs/react.yaml",
    nodes.WithKinds(myKind()),                  // static kinds
    nodes.WithRuntimeKinds(func(rt *runtime.Runtime) ([]graph.NodeKind, error) {
        return script.New(script.WithRuntime(rt))
    }),
    nodes.WithModelDecoders(decoders),          // request.extensions
    nodes.WithBlobStore(store),                 // media out of the state
)
```

The registry's shape matches the engine tables in `core/deploy`: a kind is a name
resolved at build time, so an unknown kind is a document error listing the known
names.

## Suspension

Asking is a primitive, not a node kind: any node may ask — a tool node relaying an
`ask_user` tool, a custom node collecting a missing parameter. `One node may
suspend per step`, and the step finishes first, so a suspension cannot tear a
concurrent step apart. On resume the node is re-entered from the start with
`Invoke.Answer` set; anything it did before suspending is its own idempotency
problem, which is why a node with a side effect should record it with
`Checkpoint` before suspending.

## Checkpoints and replay

`graph.NewRunner` takes a `RunnerConfig` with the store, the run's identity and
the session/task ids. The graph itself is compiled once per deployment and shared
by every session; the runner owns one run.

- Deltas are written with the step; a base carries the whole state and is written
  when the deltas would be missing, or every `checkpoint.every` steps.
- The engine's `Fingerprint` is the document's **path and bytes**: editing the
  file invalidates the checkpoints taken under the old graph, the same rule a
  changed runtime or engine setting follows.
- `Replay: true` answers model calls from the task's records. A miss is an error,
  because a replay that silently goes live is a replay that proves nothing.

## Events

Besides the events every run emits (text, reasoning, tool calls, usage), the graph
adds `NodeEvent` and — when `stream.state` is on — `GraphState`, which reports
each step's writes. Both are `agent.EventExtension` payloads, so a protocol
forwards them without knowing the graph exists.

## What replaced what

| Gone | Now |
| --- | --- |
| `core/graph`, a board of untyped vars | `core/agent-graph`, declared state keys with merge rules |
| Message channels copied per branch | Persistent message sequences, shared on append |
| `reflect.DeepEqual` + `MergeFunc` | `merge: single / last / append`, declared per key |
| Edges as bare connections | Edges carrying a compiled condition |
| A fixed node set | A registry of kind names, extended by the host |
