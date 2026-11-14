# Graph document

An engine of `kind: graph` runs the file named by `settings.path`. It is one
document, compiled once per deployment and shared by every session.

```yaml
version: 1
name: react
entry: model
input: messages

limits:
  max_steps: 16
  max_nodes: 64
  timeout: 5m
  concurrency: 4

checkpoint:
  every: 8            # also write a base every N steps

stream:
  state: false        # report each step's writes as a GraphState event

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
      output: answer
      request:
        model: {provider: luna, name: gpt-5}
        system: Use the file tools when they help.
        tools: {names: [read_file, write_file]}
  - id: tools
    kind: tool
    config: {messages: messages, pending: pending, parallelism: 4}

edges:
  - {from: model, to: tools, when: "len(pending) > 0"}
  - {from: tools, to: model}
  - {from: model, to: END, when: "len(pending) == 0"}

output: {key: answer}
```

## The run is a frontier loop

A step invokes the frontier, applies the step's writes in declaration order, then
evaluates the outgoing edges of every node that ran. A graph may loop — a ReAct
cycle is just edges — so there is no topological order and no cycle rejection.
A node reached twice in one step runs once; `limits.max_steps` bounds a loop that
never empties the frontier.

A node produces **state**, never a value on an edge. A skipped node is not
invoked but its edges still fire: it routes without running.

## Fields

| Field | Rule |
| --- | --- |
| `version` | Must be `1`. |
| `entry` | The node the first frontier holds. Required. |
| `input` | The state key `Start` seeds with the run's messages. Empty means the graph takes no input. |
| `limits` | `max_steps`, `max_nodes` (skipped nodes count too), `timeout`, `concurrency`. Zero means no cap; the caller's context is always the outer bound. |
| `checkpoint.every` | `0` writes a base only when the deltas would be missing; N also writes one every N steps. |
| `stream.state` | Emit a `GraphState` event per step. Off by default — it costs a projection per step. |
| `output.key` | The state key a finished run reports. |

## State

| Field | Values |
| --- | --- |
| `type` | `text`, `json`, `messages`, `bool`. |
| `merge` | `single` (default: a second write in one step is an error), `last`, `append`. |
| `carry` | Keep the key across the tasks of one session: a new task seeds it from the previous task's state. |

`carry` is for bounded state (a game, a form, a plan). A key that grows without
bound costs its own size on every turn, because the state is re-read from the
store: a 200-message transcript measured 667 µs and 5000 allocations per turn.

## Nodes

```yaml
- id: model          # unique in the document
  kind: model        # a registered kind; an unknown one is a build error
  config: { ... }    # the kind decodes this strictly
  skip: "step > 10"  # a condition: when it holds, the node is not invoked
  timeout: 90s       # one invocation
  retries: 2
```

### `model`

| Config | Meaning |
| --- | --- |
| `messages` | State key holding the conversation (`messages` value). |
| `pending` | State key the call's tool calls are written to. |
| `usage` | State key the token usage is appended to. |
| `output` | State key the response text is written to. |
| `request.stream` | Nil streams (incremental events); `false` returns one finished response. |
| `request.model` | `provider`, `name`, `profile`. |
| `request.system` | The system framing for this call. |
| `request.hint` | A routing hint (`selector`), not a model choice. |
| `request.tools` | `enabled`, `names`, `require`, `choice`. |
| `request.extensions` | Provider extensions, decoded by the host's decoders. |

### `tool`

| Config | Meaning |
| --- | --- |
| `messages`, `pending` | As above: read the conversation, drain the pending calls. |
| `effects` | `after` (default) records a finished call; `before` writes the started record before the tool runs, so a crash leaves an outcome to reconcile. |
| `parallelism` | Caps one batch's concurrent calls; `0` is unbounded. |

### `condition`

```yaml
- id: done
  kind: condition
  config: {key: answer, when: "answer != nil", then: yes, else: no}
```

### `script`

Not built in — the host registers the kind (`core/agent-graph/script`), because a
script engine is a dependency choice.

## Edges and conditions

An edge fires when its condition holds; an empty condition always fires. A
condition is an [expr](https://expr-lang.org) expression over the state as it was
when the step started, plus `step`:

```yaml
edges:
  - {from: model, to: tools, when: "len(pending) > 0 and step < 8"}
  - {from: tools, to: model}
  - {from: model, to: END}
```

Conditions compile once at build, so a typo is a load error. In the environment,
text is a string, a number or object is decoded JSON, a message list is the slice
(`len(messages)` works), and media is its descriptor. A condition that returns a
non-bool is a run error naming the node.
