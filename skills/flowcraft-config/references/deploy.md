# Deploy document schema

Owned by `core/deploy`. `deploy.Parse` strictly decodes JSON or YAML and
validates the document shape. The L2 validator runs exactly this:
`core/deploy.Parse` plus `core/runtime.DecodeConfig` when a `runtime`
section exists. It does not resolve factories, settings `file`/`embed`
references, or kind semantics — resource factories are registered
explicitly by the host, and the host build is where those checks happen.

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
The validator never resolves these references; resolution and strict
settings decoding happen in the host build.

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
| `sandbox.Runner` | `local`, `bwrap`, `seatbelt` | `core/sandbox/{local,bwrap,seatbelt}` |
| `inference.Provider` | `openai`, `deepseek`, `qwen`, ... | core contract; impls registered from provider drivers |
| `inference.Assembly` | `unified` | `core/inference` |
| `inference.Router` | `unified` | `core/inference/route` |
| `tool.Registry` | `memory` | `core/tool` |
| `tool.Source` | app/provider-specific | host/app |
| `tool.Assembly` | `memory`, `middleware` | `core/tool`, `core/tool/middleware` |
| `agent.ScriptRuntime` | `js`, `lua` | `core/agent/scriptrt/{jsrt,luart}` |
| `agent.Engine` | `graph` | `core/graph/resource` |
| `delegation.Service` | `local` | `core/delegation` |
| `delegation.Directory` | `local` | `core/delegation` |
| `delegation.SessionProvider` | `random` | `core/delegation` |
| `checkpoint.Store` | `workspace` | `core/agent/checkpoint/workspace` |

The table is informational: the validator accepts any kind that fits the
resource envelope, and only the host build can construct these factories.

Engine dependencies must match the graph engine's declared dep names:
`inference`, `router`, `tools`, `workspace`, `sandbox`, `script_runtime`.
