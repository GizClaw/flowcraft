---
layout: default
title: Graph Runtime
---
# Graph Runtime Guide

`sdk/graph` is the built-in graph DAG engine for FlowCraft. It compiles a
[`GraphDefinition`](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk/graph#GraphDefinition)
(JSON) into an
[`agent.Engine`](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk/agent#Engine)
and runs it wave-by-wave over a board. It is what the `graph` engine kind in
[deployment documents](deploy.md#engines) binds to.

The graph package is deliberately small: a wire format, a registry of node
types, a build step, and a runner. Behaviour lives in the node types the host
registers; topology lives in the JSON; state lives on the board passed to
`Execute`.

## Concepts

### The four layers

The package is a pipeline where each layer has exactly one representation and
types from later layers never leak back:

| Layer        | Type                               | Job                                                    |
| ------------ | ---------------------------------- | ------------------------------------------------------ |
| Wire         | `GraphDefinition`                  | serialisable JSON document; node configs opaque        |
| Registration | `Registry` + `NodeType[C]`         | binds a node type name to a typed config + handler     |
| Build        | `Build(def, reg, opts...)` → `*Graph` | validates, compiles edges/conditions, resolves roles |
| Execution    | `*Graph.Execute(ctx, run, host, board)` | advances a node frontier wave by wave             |

A `*Graph` **is** an `agent.Engine`. There is no per-run wrapper; you build
once and call `Execute` concurrently any number of times.

### Nodes are functions, not objects

Node behaviour is a `NodeType[C]` — a typed config schema plus a handler
closure. Resolved config is per-invocation data (variable references in it
depend on board state), so it is a handler parameter, not shared node state.
A node type is registered once; a `*Graph` is built once; concurrent runs
share both without locking.

### Node I/O roles

A node type declares what it reads and writes as `Role`s in its `Meta`:

- `RoleVar` — an untyped board variable (`Board.GetVar` / `SetVar`).
- `RoleMessages` — a typed message channel (`Board.Channel`).

A role's board key is either **static** (`Role.Name`) or **bound** from a
node config field (`Role.ConfigKey`) — e.g. an inference node declaring
`messages_channel` in its config. Roles are resolved once at `Build` and
enforced at invocation: required reads must exist before the handler runs,
required writes must exist after it returns.

### The board

The graph operates directly on `agent.Board`: typed message channels plus
untyped control vars, fully mutex-guarded, with snapshot/restore for
checkpoints and parallel branch isolation. The graph package defines no board
type of its own.

## First graph

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "os"

    "github.com/GizClaw/flowcraft/sdk/agent"
    "github.com/GizClaw/flowcraft/sdk/graph"
    "github.com/GizClaw/flowcraft/sdk/graph/nodes"
    "github.com/GizClaw/flowcraft/sdk/message"
)

func main() {
    ctx := context.Background()

    // 1. Register the node types you want available in graph JSON.
    reg := graph.NewRegistry()
    must(nodes.RegisterInference(reg, nodes.InferenceNodeDeps{
        Runtime: runtime, // *inference.Runtime from sdk/inference/config
    }))
    must(nodes.RegisterTool(reg, executor)) // tool.Dispatcher

    // 2. Load a graph definition. The kernel's wire form is JSON; if you
    //    unmarshal yourself, feed it JSON. The deploy factory also accepts
    //    YAML for graph.file / graph.inline and converts it before Build.
    data, err := os.ReadFile("graphs/greeter.json")
    if err != nil { panic(err) }
    var def graph.GraphDefinition
    if err := json.Unmarshal(data, &def); err != nil { panic(err) }

    // 3. Build the engine.
    g, err := graph.Build(&def, reg)
    if err != nil { panic(err) }

    // 4. Run it.
    board := agent.NewBoard()
    board.AppendChannelMessage(agent.MainChannel,
        message.NewTextMessage(message.RoleUser, "hello"))
    finalBoard, err := g.Execute(ctx, agent.Run{
        Identity: agent.Identity{AgentID: "greeter", RunID: "run-1"},
    }, host, board)
    if err != nil { panic(err) }
    fmt.Println(finalBoard.Channel(agent.MainChannel))
}

func must(err error) {
    if err != nil { panic(err) }
}
```

`graphs/greeter.json` (the wire layer):

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
        "system_prompt": "You are a friendly greeter.",
        "tool_pending_key": "tool_pending"
      }
    }
  ],
  "edges": []
}
```

The wire form is JSON; when a graph is loaded through `sdk/graph/config`
(e.g. `engine.settings.graph` in a deployment), `graph.file` and
`graph.inline` also accept YAML, which the loader converts to JSON with
`sdk/config/utils` before `Build` sees the definition.

The build step validates the definition against the registry, compiles edge
and skip conditions, statically resolves I/O roles, and returns an immutable
`*Graph` ready to run.

## Built-in node types

`sdk/graph/nodes` ships three factory pairs that compose into the classic
agent loop:

| Type        | Factory                               | Config highlights                                                              |
| ----------- | ------------------------------------- | ------------------------------------------------------------------------------ |
| `inference` | `nodes.RegisterInference(reg, deps)`  | `model`, `messages_channel`, `system_prompt`, `output_key`, `usage_key`, `tool_pending_key`, `stream`, `tools`, sampling knobs |
| `tool`      | `nodes.RegisterTool(reg, dispatcher)` | `messages_channel`, `results_key`                                               |
| `script`    | `script.Register(reg, deps)`          | `runtime`, `name`, `source`, `config`                                           |

The inference node appends one assistant message to the channel and flags
`finish_reason == tool_calls` onto the var named by `tool_pending_key`; the
tool node executes the channel tail's tool calls as one batch and appends one
`role=tool` message. A canonical inference → tool → inference loop is three
nodes connected by edges; the host registers the factories once and the JSON
describes the topology.

The script node (`sdk/graph/nodes/script`) needs a `ScriptNodeDeps` with a
`Runtimes` map; `workspace` and `commandRunner` are opt-in `fs` / `shell`
globals.

## Custom node types

A node type is `NodeType[C]` — a typed config struct, a `Meta` of I/O roles,
and a handler that takes the execution context and board:

```go
type sumConfig struct {
    A          int    `json:"a"`
    B          int    `json:"b"`
    OutChannel string `json:"out_channel"`
}

func sumHandler(ec graph.ExecutionContext, board *agent.Board, cfg sumConfig) error {
    board.AppendChannelMessage(cfg.OutChannel, message.Message{
        Role: message.RoleAssistant,
        Content: message.Content{Parts: []message.Part{
            message.TextPart{Text: fmt.Sprintf("%d", cfg.A+cfg.B)},
        }},
    })
    return nil
}

var sumNode = graph.NodeType[sumConfig]{
    Meta: graph.Meta{
        Reads: []graph.Role{
            {Kind: graph.RoleVar, Name: "go", Required: true},
        },
        Writes: []graph.Role{
            {Kind: graph.RoleMessages, ConfigKey: "out_channel", Required: true},
        },
    },
    Handler: sumHandler,
}

_ = graph.RegisterType(reg, "math.sum", sumNode)
```

`out_channel` is bound at build time from the node's config field, so the same
`sum` type can write to any channel declared in the graph. Handlers write the
board directly — there is no `NodeResult` wrapper; return `nil` for success or
a classified `errdefs` error.

## Parallel branches

Parallelism is **wave-based**: when the frontier contains multiple nodes and
the engine is built with `ParallelConfig.Enabled`, each node runs against a
private copy of the pre-fork board and merges back at the wave barrier. Two
merge strategies ship in the box:

| Strategy           | Behaviour                                                 |
| ------------------ | --------------------------------------------------------- |
| `first_write_wins` | first branch to write wins; later writes are dropped      |
| `last_write_wins`  | last branch to write wins; earlier writes are overwritten |

Per-wave limits (`branch_timeout`, `max_concurrency`, `max_branches`) are
configured under `engine.settings.build.parallel` in the deployment document
and enforced by the graph runner. Stream deltas from a branch are stamped
speculative until the wave accepts the `(ForkID, BranchID)`; scripts can
cancel a branch through the `parallel.cancelNode` bridge.

## Deploy integration

`sdk/graph/config` ships the production factory used by the `graph` engine
kind in deployment documents. It wires the standard node factories
(inference, tool, script) and exposes the canonical dependency names:

| Name             | Type                  | Required when                                |
| ---------------- | --------------------- | -------------------------------------------- |
| `inference`      | `inference.Assembly`  | graph contains an inference node             |
| `tools`          | `tool.Assembly`       | graph contains tool nodes or inference tools |
| `workspace`      | `workspace.Workspace` | scripts need filesystem access               |
| `sandbox`        | `sandbox.Runner`      | scripts need command execution               |
| `script_runtime` | `agent.ScriptRuntime` | graph contains a script node                 |

See [deploy.md](deploy.md#engines) for the engine settings schema, graph
definition forms (scalar / `file` / `inline`), and full configuration
options. The factory loads graph files itself (`WithBaseDir`, 1 MiB cap) —
`graph.ParseDefinitionFile` does not exist.

## Testing

The graph runner is hermetic — no network I/O. The standard test pattern is
"unmarshal a JSON literal, build, hand a board, assert on the result":

```go
var def graph.GraphDefinition
if err := json.Unmarshal([]byte(`{
  "name": "t",
  "entry": "say",
  "nodes": [
    { "id": "say", "type": "echo",
      "config": { "text": "hi", "out": "main" } }
  ],
  "edges": []
}`), &def); err != nil {
    t.Fatal(err)
}

reg := graph.NewRegistry()
must(graph.RegisterType(reg, "echo", echoNode))

g, err := graph.Build(&def, reg)
if err != nil { t.Fatal(err) }

board := agent.NewBoard()
final, err := g.Execute(ctx, agent.Run{
    Identity: agent.Identity{RunID: "test-run"},
}, agent.NoopHost{}, board)
if err != nil { t.Fatal(err) }

msgs := final.Channel("main")
if got := msgs[len(msgs)-1].Content.Parts[0].(message.TextPart).Text; got != "hi" {
    t.Fatalf("got %q, want hi", got)
}
```

For node-level unit tests, call the handler directly with a `graph.ExecutionContext`
and a hand-built board; the surrounding graph machinery is irrelevant once the
type is registered.

## Further reading

- Package contract: `sdk/graph/doc.go` (the four layers in detail).
- Standard node types: `sdk/graph/nodes/doc.go`,
  `sdk/graph/nodes/script/node.go`.
- Production factory: `sdk/graph/config/factory.go`.
- Board and run contract: `sdk/agent/board.go`, `sdk/agent/execute.go`
  (`Run` / `Identity`).
- Engine wiring in deployment documents:
  [deploy.md#engines](deploy.md#engines).
