# Deploy document schema

Owned by `sdkx/deploy`. Strictly decoded: unknown fields, unknown
kinds/impls, unregistered engines/hooks, dep type mismatches, dead
resources, and cycles all fail `Parse`/`Build`.

The filename is arbitrary — `deploy.yaml` is only the convention used by
this repo's examples and this skill's template. Pass any path to the
validator.

## Top-level

```yaml
version: v1
resources: {<name>: <ResourceEntry>}
agents: {<id>: <AgentEntry>}
runtime: {<opaque, decoded strictly by sdkx/runtime>} # optional
```

## ResourceEntry

```yaml
<name>:
  kind: <category>        # matched against DepSpec.Type when bound whole
  impl: <implementation>  # selects a registered factory
  export: false           # true = retrievable via deploy.ResourceAs even if unused
  deps: {<factory dep name>: <DepRef>}
  settings: <impl-owned, strictly decoded>
```

`settings` accepts exactly one form:

```yaml
settings: {file: ./inference.yaml}   # external file
settings: {embed: assets/x.yaml}     # embedded asset
settings: <literal content>          # plain string or nested object
```

A reference object must use exactly `file`/`embed` keys; anything else is
treated as the document itself.

## DepRef

```yaml
deps:
  inference: infer          # whole resource
  workspace: ws/project     # one item inside a container resource
  tools: {source: host.shared, ref: default} # host-owned value
```

Scalars always mean resources. Sources are deliberately verbose and are
never closed by `Result.Close`.

## AgentEntry

```yaml
<id>:
  source: {file: ./agents/x.agent.yaml}  # XOR inline fields below
  card: {name: X, description: ...}
  tools: [allow, list]        # runtime policy gate -> Run.ToolAllowList
  engine: {kind: graph, settings: {...}}
  deps: {<engine dep name>: <DepRef>}
  prepare: [{type, deps, settings}]
  referees: [[same shape]]
  commit: [[same shape]]
  observe: [[same shape]]
  policy: {max_revise: N, artifact_channels: [...]}
```

`policy` is per-call harness state (`WithMaxRevise`/`WithArtifactChannels`);
engine factories never read it. `tools` is NOT build-time validated against
the catalog. Agent recipe files require their own `version: v1` and reject
unknown fields.

## First-party resource kinds

| Kind | Impl | Built value | Package |
| --- | --- | --- | --- |
| `workspace.Registry` | `yaml` | container, item `workspace.Workspace` | `sdk/workspace/config` |
| `sandbox.Registry` | `yaml` | container, item `sandbox.Runner` | `sdk/sandbox/config` |
| `inference.Assembly` | `yaml` | runtime + optional router, item `inference.Runtime` | `sdk/inference/config` |
| `tool.Assembly` | `yaml` | catalog + executor; optional `deps.sandbox` (`sandbox.Runner`) for sandbox-backed exec tools | `sdk/tool/config` |
| `event.Bus` | `memory` | in-process bus | `sdk/event/config` |
| `agent.ScriptRuntime` | `js` / `lua` | script runtime | `sdkx/agent/script/{jsrt,luart}` |
| `scheduler.Server` | `local` | unstarted scheduler server | `sdk/scheduler/config` + `sdkx/scheduler` |
| `delegation.AsyncBackend` | `kanban-memory` | async delegation backend | `sdkx/delegation/kanban/config` |
| `agent.CheckpointStore` | `sqlite` | checkpoint store | `sdkx/agent/checkpoint/sqlite/config` |

Engine kinds: `graph` (built-in), `a2a` (remote proxy).

`memory.Assembly` resources are implementation-registered: each
implementation module ships its own factory and settings schema (the
flowcraft `memory/` module is not released yet, so this skill omits
memory implementation examples). The `sdk/memory` contract and the
`sdkx/memory/hook` factories are released.

Only `discard_on_interrupt` referee is built in. `memory.context`
(preparer), `memory.turn` (committer), and `delegation_handoff` (referee)
are first-party and registered by the host/validator.

## Registration (Go)

```go
loader := sdkconfig.NewLoader(sdkconfig.WithBaseDir(configDir))
builder := deploy.NewBuilder(deploy.WithLoader(loader))
builder.RegisterEngine(graphconfig.NewFactory(graphconfig.WithLoader(loader)))

// Resource factories: builders register directly (they implement config.Factory).
workspaceBuilder := workspaceconfig.NewBuilder(workspaceconfig.Deps{BaseDir: configDir})
builder.MustRegisterResource(workspaceBuilder)
sandboxBuilder := sandboxconfig.NewBuilder(sandboxconfig.Deps{})
builder.MustRegisterResource(sandboxBuilder)
toolBuilder := toolconfig.NewBuilder(toolconfig.Deps{})
builder.MustRegisterResource(toolBuilder)

builder.MustRegisterResource(inferenceconfig.NewDeployFactory(providerFactories, secretResolvers))
builder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())
builder.MustRegisterResource(jsrt.NewDeployFactory())
builder.MustRegisterResource(luart.NewDeployFactory())
// Memory implementation factories register here once released (e.g.
// builder.MustRegisterResource(myMemory.Factory())).
```

`runtime.NewBuilder(deployBuilder)` marks runtime-referenced resources as
external consumers; plain `deploy.Builder.Build` rejects a runtime-only
resource as dead configuration unless you pass
`deploy.WithExternalResourceConsumers(names...)`.

## Build rules that fail fast

- Unsupported `version`, unknown fields anywhere, missing `kind`/`impl`.
- Unregistered factories, engines, sources, hook types.
- Dep names not in the consumer's `Spec`; dep type mismatches; item refs on
  non-containers; cycles; dead configuration.
- Typed-nil resources and deps.
- First error short-circuits — fix one error at a time.

## Sources of truth

`sdkx/deploy/document.go`, `sdkx/deploy/builder.go`, `sdkx/deploy/doc.go`,
`docs/guides/deploy.md`.
