# Deploy document schema

Owned by `core/deploy`. `deploy.Parse` strictly decodes JSON or YAML and
validates the document shape. Resource factories are registered explicitly
by the host.

## Top-level

```yaml
version: v1
resources: {<name>: <ResourceEntry>}
agents: {<id>: <AgentEntry>}
runtime: {<opaque, decoded strictly by core/runtime>} # optional
```

## ResourceEntry

```yaml
<name>:
  kind: <Kind>        # e.g. event.Bus, tool.Assembly
  impl: <Impl>        # selects a registered factory
  deps: {<dep name>: <Ref>}
  settings: <factory-owned literal or {file|embed}>
```

`settings` accepts inline JSON/YAML-compatible content, `{file: path}`, or
`{embed: name}`. A file/embed reference must be the whole settings subtree.

## Ref

```yaml
deps:
  inference: infer          # whole resource
  workspace: ws/project     # one item exported by a container resource
```

Refs have at most one slash and are validated by `core/resource`.

## AgentEntry

```yaml
<id>:
  card: {name: X, description: ...}
  tools: [allow, list]
  engine:
    kind: agent.Engine
    impl: graph
    deps: {inference: infer}
    settings: {graph: {file: ./graphs/assistant.yaml}}
  prepare: [{type, deps, settings}]
  observe: [{type, deps, settings}]
  referees: [{type, deps, settings}]
  commit: [{type, deps, settings}]
  policy: {max_revise: N, artifact_channels: [...]}
```

Engine dependencies are under `engine.deps`; top-level `deps` is not part of
the core schema. Agent hooks use factory kind `hook.<slot>`.

## First-party core kinds

| Kind | Common impls | Package |
| --- | --- | --- |
| `event.Bus` | `memory` | `core/event` |
| `workspace.Workspace` | `local` | `core/workspace` |
| `sandbox.Runner` | `local` | `core/sandbox` |
| `inference.Provider` | `openai`, `deepseek`, `qwen`, ... | `driver/<name>` |
| `inference.Assembly` | `unified` | `core/inference` |
| `tool.Source` | app/provider-specific | host/app |
| `tool.Assembly` | `memory` | `core/tool` |
| `agent.ScriptRuntime` | `js`, `lua` | `core/agent/scriptrt/{jsrt,luart}` |
| `agent.Engine` | `graph` | `core/graph/resource` |
| `delegation.Service` | `local` | `core/delegation` |
| `agent.CheckpointStore` | workspace/sqlite | `core/agent/checkpoint/workspace`, `integrations/sqlite` |

Engine dependencies must match the graph engine's declared dep names:
`inference`, `router`, `tools`, `workspace`, `sandbox`, `script_runtime`.
