---
layout: default
title: Graph Runtime
---
# Graph Runtime Guide

`sdk/graph` is the built-in graph DAG engine for FlowCraft. It compiles
a [`GraphDefinition`](../reference/../) (JSON) into an
[`agent.Engine`](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk/agent#Engine)
and runs it wave-by-wave over a board. It is what the
`graph` engine kind in
[deployment documents](deploy.md#engines) binds to.

The graph package is deliberately small: a wire format, a registry of
node types, a build step, and a runner. Behaviour lives in the node
types the host registers; topology lives in the JSON; state lives on
the board passed to `Execute`.

## Concepts

### The four layers

The package is a pipeline where each layer has exactly one
representation and types from later layers never leak back:

| Layer        | Type                               | Job                                                    |
| ------------ | ---------------------------------- | ------------------------------------------------------ |
| Wire         | `GraphDefinition`                  | serialisable JSON document; node configs opaque        |
| Registration | `Registry` + `NodeType[C]`         | binds a node type name to a typed config + handler     |
| Build        | `Build(...)` → `*Graph`            | validates, compiles edges, returns an immutable engine |
| Execution    | `*Graph.Execute(ctx, board, host)` | advances a node frontier wave by wave                  |

A `*Graph` **is** an `agent.Engine`. There is no per-run wrapper; you
build once and call `Execute` concurrently any number of times.

### Nodes are functions, not objects

Node behaviour is a `NodeType[C]` — a typed config schema plus a
handler closure. Resolved config is per-invocation data (variable
references in it depend on board state), so it is a handler parameter,
not shared node state. A node type is registered once; a `*Graph` is
built once; concurrent runs share both without locking.

### Node I/O roles

A node type declares what it reads and writes as `Role`s in its
`Meta`:

- `RoleVar` — an untyped board variable (`Board.GetVar` / `SetVar`).
- `RoleMessages` — a typed message channel (`Board.Channel`).

A role's board key is either **static** (fixed name) or **bound**
from a node config field — e.g. an LLM node declaring
`messages_channel` in its config. Roles are resolved once at `Build`
and enforced at invocation: required reads must exist before the
handler runs, required writes must exist after it returns.

### The board

The graph operates directly on `agent.Board`: typed message channels
plus untyped control vars, fully mutex-guarded, with snapshot/restore
for checkpoints and parallel branch isolation. The graph package
defines no board type of its own.

## First graph

```go
package main

import (
    "context"
    "fmt"

    "github.com/GizClaw/flowcraft/sdk/agent"
    "github.com/GizClaw/flowcraft/sdk/graph"
    "github.com/GizClaw/flowcraft/sdk/graph/nodes"
    "github.com/GizClaw/flowcraft/sdkx/agent/graph"
    "github.com/GizClaw/flowcraft/sdk/inference/config"
    // ...
)

func main() {
    ctx := context.Background()

    // 1. Register the node types you want available in graph JSON.
    reg := graph.NewRegistry()
    must(nodes.RegisterInference(reg, inferenceDeps))
    must(nodes.RegisterTool(reg, dispatcher))

    // 2. Load a graph definition (the same shape goes in
    //    deployment documents' engine.settings.graph).
    def, err := graph.ParseDefinitionFile("graphs/greeter.json")
    if err != nil { panic(err) }

    // 3. Build the engine.
    g, err := graph.Build(ctx, reg, def)
    if err != nil { panic(err) }

    // 4. Run it.
    board := agent.NewBoard()
    out, err := g.Execute(ctx, board, host)
    if err != nil { panic(err) }
    fmt.Println(out)
}
```

`graph.json` (the wire layer):

```json
{
  "name": "greeter",
  "entry": "ask",
  "nodes": [
    {
      "id": "ask",
      "type": "inference",
      "config": {
        "model": { "provider": "openai", "name": "gpt-5.4" },
        "messages_channel": "main",
        "system": "You are a friendly greeter."
      }
    }
  ],
  "edges": []
}
```

The build step validates the definition against the registry, compiles
edge and skip conditions, statically resolves I/O roles, and returns
an immutable `*Graph` ready to run.

## Built-in node types

`sdk/graph/nodes` ships three factory pairs that compose into the
classic agent loop:

| Type        | Factory                               | Reads                             | Writes                                                    |
| ----------- | ------------------------------------- | --------------------------------- | --------------------------------------------------------- |
| `inference` | `nodes.RegisterInference(reg, deps)`  | messages channel + model          | messages channel (appends assistant) + `tool_pending` var |
| `tool`      | `nodes.RegisterTool(reg, dispatcher)` | messages channel + `tool_pending` | messages channel (appends tool results)                   |
| `script`    | `script.Register(reg, runtime)`       | vars + sandbox                    | vars + sandbox                                            |

A canonical inference → tool → inference loop is just three nodes
connected by edges; the host registers the three factories once and
the JSON describes the topology.

## Custom node types

A node type is `NodeType[C]` — a typed config struct, a `Meta` of I/O
roles, and a handler that takes a `Board` and a `Host`:

```go
type sumConfig struct {
    A          int    `json:"a"`
    B          int    `json:"b"`
    OutChannel string `json:"out_channel"`
}

func sumHandler(ctx context.Context, hctx graph.NodeContext, cfg sumConfig) (graph.NodeResult, error) {
    ch, err := hctx.Board.Channel(cfg.OutChannel)
    if err != nil { return graph.NodeResult{}, err }
    if err := ch.Append(agent.Message{
        Role: agent.RoleAssistant,
        Parts: []agent.Part{agent.TextPart{
            Text: fmt.Sprintf("%d", cfg.A+cfg.B),
        }},
    }); err != nil { return graph.NodeResult{}, err }
    return graph.NodeResult{}, nil
}

var sumNode = graph.NodeType[sumConfig]{
    Meta: graph.Meta{
        Reads: []graph.Role{
            {Kind: graph.RoleVar, Name: "go", Required: true},
        },
        Writes: []graph.Role{
            {Kind: graph.RoleMessages, Name: "<from config>", ConfigKey: "out_channel", Required: true},
        },
    },
    Handle: sumHandler,
}

func init() {
    _ = graph.RegisterType(myReg, "math.sum", sumNode)
}
```

`out_channel` is bound at build time from the node's config field, so
the same `sum` type can write to any channel declared in the graph.

## Parallel branches

Graphs can branch in parallel via a `parallel` edge marker or a
declarative parallel block. Each branch executes its own sub-frontier
and merges back at a barrier. Two merge strategies ship in the box:

| Strategy           | Behaviour                                                 |
| ------------------ | --------------------------------------------------------- |
| `first_write_wins` | first branch to write wins; later writes are dropped      |
| `last_write_wins`  | last branch to write wins; earlier writes are overwritten |

Per-branch limits (`max_concurrency`, `branch_timeout`,
`max_branches`) are configured under `engine.settings.build.parallel`
in the deployment document and enforced by the graph runner.

## Deploy integration

`sdkx/agent/graph` ships the production factory used by the
`graph` engine kind in deployment documents. It wires the standard
node factories (inference, tool, script) and exposes the canonical
dependency names:

| Name             | Type                  | Required when                                |
| ---------------- | --------------------- | -------------------------------------------- |
| `inference`      | `inference.Assembly`  | graph contains an inference node             |
| `tools`          | `tool.Assembly`       | graph contains tool nodes or inference tools |
| `workspace`      | `workspace.Workspace` | scripts need filesystem access               |
| `sandbox`        | `sandbox.Runner`      | scripts need command execution               |
| `script_runtime` | `agent.ScriptRuntime` | graph contains a script node                 |

See [deploy.md](deploy.md#engines) for the engine settings schema,
graph definition forms (scalar / `file` / `inline`), and full
configuration options.

## Testing

The graph runner is hermetic — no network I/O. The standard test
pattern is "build a graph from a JSON literal, hand a board, assert on
the result":

```go
def, err := graph.ParseDefinition([]byte(`{
  "name": "t",
  "entry": "say",
  "nodes": [
    { "id": "say", "type": "echo",
      "config": { "text": "hi", "out": "main" } }
  ],
  "edges": []
}`))
if err != nil { t.Fatal(err) }

reg := graph.NewRegistry()
must(graph.RegisterType(reg, "echo", echoNode))

g, err := graph.Build(ctx, reg, def)
if err != nil { t.Fatal(err) }

board := agent.NewBoard()
_, err = g.Execute(ctx, board, agent.NoopHost{})
if err != nil { t.Fatal(err) }

ch, _ := board.Channel("main")
if got := ch.Tail().Parts[0].(agent.TextPart).Text; got != "hi" {
    t.Fatalf("got %q, want hi", got)
}
```

For node-level unit tests, build a fixture `NodeContext` with a
hand-built board and a `NoopHost`, then call the handler directly. The
surrounding graph machinery is irrelevant once the type is registered.

## Further reading

- Package contract: `sdk/graph/doc.go` (the four layers in detail).
- Standard node types: `sdk/graph/nodes/doc.go`,
  `sdk/graph/nodes/script/doc.go`.
- Production factory: `sdkx/agent/graph/factory.go`.
- Turn harness that calls into the engine:
  [`sdk/agent/doc.go`](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk/agent#Engine).
- Engine wiring in deployment documents:
  [deploy.md#engines](deploy.md#engines).
