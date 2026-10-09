<div align="center">

# FlowCraft

**A modular Go toolkit for extensible AI applications, long-term memory, provider backends, and local interactive workflows.**

[![CI](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml/badge.svg)](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/GizClaw/flowcraft/core.svg)](https://pkg.go.dev/github.com/GizClaw/flowcraft/core)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

FlowCraft is a Go workspace for building and evaluating AI applications without
tying application code to one model provider or execution model. Graphs are one
built-in option, not a required architecture: use the core packages directly, or
start with the forge demo in `examples/forge` for a runnable local workspace, or
the anvil example in `examples/anvil` for a minimal `craft` host application.

## Modules

- **`core`** — The single platform module: agent execution, graph, tool,
  model, message, inference, memory contracts, event bus, telemetry,
  workspace, sandbox, deployment/resource assembly, runtime, sessions, and
  delegation contracts.
- **`craft`** — Application assembly on top of `core`: the `craft.yaml`
  definition, compile-time capabilities, the MCP plugin host (plugin processes
  as MCP servers, host primitives as MCP tools for plugins), tools / graph
  nodes / skills / hooks / UI contributed by plugins, and the process-level
  `craft/manager` lifecycle.
- **`driver/*`** — Provider adapters built on `core`: OpenAI (serving the
  whole OpenAI wire family), Anthropic (the Messages family), ByteDance, and
  MiniMax.
- **`backends/*`** — Platform-specific implementations: SQLite checkpoints
  (`backends/checkpoint`) and the long-term memory backend with its eval
  harness (`backends/memory`); the sandbox backends (`bwrap`, `seatbelt`) live
  in `core/sandbox`.
- **`examples/forge`** — A runnable local workspace demo built on the current
  stack: native deploy/inference/memory scenario configs, an interactive TUI,
  scripted tests, and raid × persona simulation.
- **`examples/anvil`** — The runnable `craft` example: a minimal host
  application that assembles a Craft from a `craft.yaml` definition, a
  capability and a plugin directory, then walks the lifecycle — keyed runtimes,
  tool calls, reload, plugin hot-plug. No provider, no network.
- **`tools/releasegate`** — Release automation: changeset validation, release
  planning, and changelog aggregation.

Library layers are independently versioned Go modules; applications adopt only
the layers they need.

## Memory architecture

`core/memory` defines the memory capability contracts — `ContextProvider`,
`TurnSink`, `DocumentSink`, `ContextRenderer`, `Scope`, and `Turn`.
`core/memory` also provides the generic glue: a
`memory.Assembly` deploy resource that dispatches to implementations by
`impl:` name, the `memory.context` / `memory.turn` agent-lifecycle hooks, and
the GoTemplate context renderer.

This mirrors the inference pattern: `core/inference` is generic, and each
provider driver (`openai`, `anthropic`, `bytedance`, `minimax`) is a
registered factory. A deployment points one of them at any endpoint speaking
that wire family — DeepSeek and GLM ride the OpenAI driver, for example — so
the document's provider id (`openai`, `deepseek`, …) names the deployment, not
the module. Memory implementations plug in the same way — each registers under
its own `impl:` name with its own parameters (the flowcraft memory module is
one such app-registered implementation); `core` owns only the contracts and
glue.

## Quickstart

### Run the forge workspace demo

The fastest way to explore the stack is the runnable demo in
[`examples/forge`](examples/forge/):

```bash
cd examples/forge
go run . help

# Create a workspace from the werewolf scenario and run a scripted test.
go run . workspace create --config werewolf --workspace ./workspace
go run . test -test werewolf/opening_setup
```

The demo builds workspaces from native deploy/inference/memory scenario
documents, opens an interactive TUI, and runs scripted tests and raid × persona
simulations. Command reference, scenario layout, and credentials live in
[`examples/forge/README.md`](examples/forge/README.md) (中文版:
[`examples/forge/README_zh.md`](examples/forge/README_zh.md)).

### Run the craft example

[`examples/anvil`](examples/anvil/) is a minimal host application built on
`craft`: it assembles a Craft from a definition, a capability and a plugin, and
walks the lifecycle (keyed runtimes, tool calls, reload, hot-plug). No provider
or network needed:

```bash
cd examples/anvil
go run .
```

See [`examples/anvil/README.md`](examples/anvil/README.md) (中文版:
[`examples/anvil/README_zh.md`](examples/anvil/README_zh.md)) for the tour step
by step.

### Embed FlowCraft in a Go service

Use `core` directly and add `driver/*` or `backends/*` for provider
adapters and platform backends.
Assemble a deployment from `deploy.yaml` with `core/deploy`, run it with
`core/runtime`, and drive turns through `core/runtime/session`:

```go
document, _ := deploy.Parse(deployYAML)
app, _ := runtimeBuilder.Build(ctx, document)
defer app.Close()

lease, _ := app.Sessions().Open(ctx, session.Key{
    AgentID:   "assistant",
    ContextID: "conversation-1",
})
turn, _ := lease.Session().Start(ctx, agent.Request{
    Message: message.NewTextMessage(message.RoleUser, "hello"),
}, session.SinkSpec{ID: "console", Sink: streamSink})
result, _ := turn.Wait(ctx)
```

See [`docs/guides/deploy.md`](docs/guides/deploy.md) and
[`docs/guides/runtime.md`](docs/guides/runtime.md) for the full assembly and
session contracts.

### Assemble a Craft

`craft` is the layer above a deployment document: one `craft.yaml` describes the
application, a set of compile-time capabilities extends it, and every runtime is
built from the layers they compose. A minimal definition inlines its deployment
document:

```yaml
craft:
  id: notes
  name: Notes
  version: 0.1.0
deploy:
  version: v1
  resources:
    bus:
      kind: event.Bus
      impl: memory
  runtime:
    event_bus: bus
```

Each capability registers the resource factories its documents reference, so a
Craft needs at least the ones its deployment declares:

```go
type appCapability struct{}

func (appCapability) Name() string { return "app" }

func (appCapability) Register(registry *resource.Registry) error {
    registry.MustRegister(event.NewFactory())
    return nil
}
```

`craft.New` validates the definition and builds the shared services; runtimes
are opened explicitly and are keyed by an application-defined value:

```go
def, err := craft.ParseDefinition(craftYAML)
if err != nil {
    return err
}
c, err := craft.New(def, craft.Options{
    ConfigDir:    configDir,
    DataDir:      dataDir,
    Capabilities: []craft.Capability{appCapability{}},
})
if err != nil {
    return err
}
defer c.Close()

if err := c.Start(ctx); err != nil {
    return err
}
rt, err := c.OpenRuntime(ctx, craft.DefaultKey, craft.RuntimeOptions{})
```

The runtime is the same `core/runtime` one shown above; craft owns the layers,
the plugin host, the host primitives and the reload/drain lifecycle around it.
See [`docs/guides/craft.md`](docs/guides/craft.md) for the definition schema,
the plugin manifest and permissions, the host-primitive protocol, and
`craft/manager`.

## Architecture

`core` defines the execution contracts: `core/agent` owns the execution
primitives (`Engine`, `Host`, `Board`, `Run`, `Interrupt`, `Checkpoint`),
while `core/graph` compiles declarative graphs into `agent.Engine`
implementations. Memory and provider adapters compose those contracts
without becoming dependencies of the core.

```
                ┌──────────────────────┐
                │   Your application   │
                └──────────┬───────────┘
                           │
             ┌─────────────┬─────────────┐
             ▼                           ▼
      ┌─────────────┐             ┌─────────────────┐
      │ driver/*    │             │ app-registered  │
      │  inference  │             │ memory          │
      │ providers   │             │ implementations │
      │             │             │                 │
      └──────┬──────┘             └──────┬──────────┘
             └─────────────┬─────────────┘
                           ▼
                ┌──────────────────────┐
                │         core/        │
                │   agent · graph ·    │
                │   tool · event ·     │
                │  message · inference │
                │  deploy · runtime    │
                └──────────────────────┘
```

**Layering rule:** execution contracts live in `core/agent` (`agent.Engine`,
`agent.Host`, `agent.Board`) and stay leaves of the core — agent does not
import graph or tool packages. `core/graph` builds on those contracts and
returns an `agent.Engine`. Memory contracts live in core, while
app-registered implementations and adapters (`driver/*`, `backends/*`) stay outside
the core and depend on it, never the reverse.

`craft` is the optional assembly layer above that picture — it owns the
definition, the compile-time capabilities, the plugin host and the host
primitives, and hands `core/deploy` documents to `core/runtime`:

```
        Your application
               │  craft.yaml + capabilities + plugins
               ▼
      ┌──────────────────┐
      │      craft       │  layers · plugin host · host primitives · manager
      └────────┬─────────┘
               │  one deploy.Document per runtime key
               ▼
      ┌──────────────────┐
      │      core        │  deploy · runtime · sessions · agent · graph · tool
      └──────────────────┘
```

## Module map

| Path                                                  | Role                                                                                     | Distribution         |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------- | -------------------- |
| [`core`](core/)                                       | Agent, graph, tool, model, message, inference, memory, event, telemetry, deploy, runtime | Versioned Go module  |
| [`craft`](craft/)                                     | Application assembly: craft.yaml, capabilities, plugin host, host primitives, manager     | Versioned Go module  |
| [`driver`](driver/)                                   | Provider inference adapters                                                              | Versioned Go modules |
| [`backends`](backends/)                               | SQLite checkpoints, the long-term memory backend (sandbox backends live in `core/sandbox`) | Versioned Go modules |
| [`examples/forge`](examples/forge/)                   | Runnable local workspace demo                                                            | Examples             |
| [`examples/anvil`](examples/anvil/)                   | Minimal `craft` host application (keyed runtimes, plugins, reload)                       | Examples             |
| [`tools/releasegate`](tools/releasegate/)             | Release automation                                                                       | Tools                |
| [`skills/flowcraft-config`](skills/flowcraft-config/) | Codex skill for authoring and validating FlowCraft configs                               | Codex skill          |

## Highlights

### Memory contracts (`core/memory`)

- Memory as a pluggable implementation: concrete implementations are
  app-registered behind the `core/memory` contracts.

### Streaming, durable, resumable (`core/agent`)

- `Subject`-routed event bus — every step emits structured envelopes.
- `Checkpoint` / `CheckpointStore` contracts — pause and resume an agent
  across restarts.
- `Interrupt` / `Wait` semantics that compose cleanly with `context.Context`.

### Unified inference runtime (`core/inference` + `driver/*`)

- One runtime for Generate / Embed / Transcription (Realtime reserved), with exact
  `ModelRef` addressing and compile-time capability checks.
- Providers registered as factories: OpenAI (OpenAI, Azure, DeepSeek, Kimi and
  compatible gateways), Anthropic (Anthropic and compatible Messages
  endpoints), ByteDance, and MiniMax.

### Application assembly and plugins (`craft`)

- Two extension axes with a hard boundary: **capabilities** are Go code
  registered at compile time (resource factories, host-primitive services,
  layers, resolvers, host decorators, runtime binding), while **plugins** carry
  no Go code and contribute their manifest, data, UI bundle, skills, hooks, MCP
  tools and graph nodes. A plugin can never register a resource kind; a
  declaration that arrives without its permission is dropped fail-closed.
- **MCP on both sides**: each plugin process is an MCP server whose tools reach
  agents through the shared tool set, and `craft/hostmcp` exposes the host
  primitives as MCP tools — behind a per-plugin bearer token — for the plugin to
  call back into the host.
- **Layered configuration**: definition layers, craft-level layers and
  per-runtime layers merge into one `deploy.Document` per runtime key, with the
  `${craft:...}` scheme feeding craft-wide and per-runtime values.
- **Process lifecycle**: `craft/manager` adds definition lookup, profiles, a
  single-instance lock, a state machine, blue-green `Replace`, and `Group` for
  several Crafts in one process.

### Runnable local workspace demo (`examples/forge`)

A runnable demo on the current stack: native scenario documents, an interactive
TUI, scripted tests with per-turn metrics, and raid × persona simulation. See
[`examples/forge/README.md`](examples/forge/README.md) for details.

### Minimal craft host application (`examples/anvil`)

The runnable `craft` example: one small host application that assembles a Craft
from a `craft.yaml` definition, one compile-time capability and one plugin
directory, then walks the lifecycle a real shell drives — scanning plugins,
opening keyed runtimes, calling tools through the runtime's assembly, reloading
one runtime, hot-plugging a plugin and shutting down. Everything is local and
deterministic. See [`examples/anvil/README.md`](examples/anvil/README.md).

## Documentation

The canonical reference is the per-package `doc.go` files, browsable on
pkg.go.dev. Topic guides live in [`docs/guides/`](docs/guides/):

- [Graph Runtime](docs/guides/graph.md) — `core/graph`: declarative DAG
  engine, node I/O roles, parallel branches, custom node types.
- [Tool System](docs/guides/tool.md) — `core/tool`: LLM function-calling
  contract, the Registry / Catalog / Executor split, middleware chain, and
  the MCP bridge.
- [Event Bus](docs/guides/event.md) — `core/event`: subject-routed
  publish/subscribe, in-process `MemoryBus`, host capability wiring,
  backpressure policies.
- [Workspace](docs/guides/workspace.md) — `core/workspace`: per-run
  filesystem abstraction, backends, capabilities, and the
  `state vs policy` split vs Sandbox.
- [Sandbox](docs/guides/sandbox.md) — `core/sandbox`: agent execution
  boundary, env / net / resources policy, runners (local / seatbelt /
  bwrap), decorators, and approval.
- [Inference Runtime](docs/guides/inference.md) — unified Generate / Embed /
  Transcription (Realtime reserved): deployment config, routing, extensions,
  streaming, media intents, hot reload.
- [Deployment Assembly](docs/guides/deploy.md) — `core/deploy`: one YAML
  document + one `Build` call to wire shared resources, named agents,
  engines, and lifecycle hooks.
- [Application Runtime](docs/guides/runtime.md) — `core/runtime` +
  `core/runtime/session`: process-level services and leased, interruptible
  streaming sessions above a built deployment.
- [Memory Stack](docs/guides/memory.md) — the three-layer memory stack:
  `core/memory` contracts and deploy/runtime glue.
- [Prompt Lifecycle Events](docs/guides/prompt.md) — the
  `agent.run.<id>.prompt.*` lifecycle events UI consumers subscribe to.
- [Delegation](docs/guides/delegation.md) — `core/delegation`: backend-neutral
  target discovery, sync / async execution, and the session-bound
  delegation lifecycle.
- [Craft Assembly](docs/guides/craft.md) — `craft`: the `craft.yaml`
  definition and capability set, the plugin manifest and permissions, the
  host-primitive protocol, and the process-level manager.

### Configuration authoring skill (`skills/flowcraft-config`)

[`skills/flowcraft-config/`](skills/flowcraft-config/) is a Codex skill for
writing, validating, and troubleshooting FlowCraft deployment
configuration: the deployment document (the filename is arbitrary;
`deploy.yaml` is the convention), the `runtime` section,
inference/workspace/sandbox/tool sub-documents, `core/memory` contracts,
and graph JSON node wiring. Concrete memory implementations are app-registered.

The skill ships an L2 dry-run validator that pins the released
`core`/`driver`/`backends` module versions and builds your deployment
through the real `core/deploy` assembly layer with stub secrets — no network, no real
credentials:

```bash
skills/flowcraft-config/scripts/validate-config.sh deploy.yaml
skills/flowcraft-config/scripts/validate-config.sh --type graph graphs/assistant.json
```

`--type` supports `deploy` (default), `inference`, `workspace`, `sandbox`,
`tool`, `graph`, and `agent` entries.

Install it into Codex (replaces an existing copy, so remove
`~/.codex/skills/flowcraft-config` first if present):

```bash
python3 ~/.codex/skills/.system/skill-installer/scripts/install-skill-from-github.py \
  --repo GizClaw/flowcraft --path skills/flowcraft-config --ref main
```

The `skills/<skill-name>/SKILL.md` layout is also discoverable through
skills.sh (`npx skills add GizClaw/flowcraft`).

Reference material:

- [`docs/`](docs/index.md) — docs landing page (guides + migration notes).
- [pkg.go.dev/github.com/GizClaw/flowcraft/core](https://pkg.go.dev/github.com/GizClaw/flowcraft/core) — platform contracts.
- [pkg.go.dev/github.com/GizClaw/flowcraft/driver/openai](https://pkg.go.dev/github.com/GizClaw/flowcraft/driver/openai) — example provider adapter.
- [pkg.go.dev/github.com/GizClaw/flowcraft/core/sandbox](https://pkg.go.dev/github.com/GizClaw/flowcraft/core/sandbox) — sandbox backends.

## Status

The active project surface is `core`, `craft`, `driver/*`, `backends/*`, and the
forge demo. `core` and `craft` are the two release-managed modules and both
remain pre-1.0; `craft` is still an unreleased module in the workspace (its
first tag ships with its first changeset), and the current `craft` surface is
the definition, capabilities, the plugin host with the v1 host primitives, and
`craft/manager` — the plugin install/update surface and the shell's UI
rendering live outside it. Durable execution contracts (checkpoints,
interrupt/resume, scheduling), OTel instrumentation, and retrieval end-to-end
coverage are maintained in-tree; checkpoint persistence is host-provided
through the `CheckpointStore` contract.

API surface is governed by SemVer per module. Breaking changes may ship as
minor bumps until a module reaches `v1.0.0`.

## Building from source

```bash
git clone https://github.com/GizClaw/flowcraft
cd flowcraft

make help          # list every target
make ci            # vet + test for all in-tree modules
make release-check # validate changesets and the pending release plan
```

This repository is a Go workspace. Active members are `core`, `craft`,
`driver/*`, `backends/*`, `examples/forge`, and `examples/anvil`; release
tooling in `tools/releasegate` builds standalone with `GOWORK=off`.

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `make ci` should be green.
2. `gofmt -l .` should print nothing.
3. Tests for new features. New behaviour without a test won't merge.
4. Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`,
   `refactor:`, `test:`, `chore:`).

Library releases are declared explicitly with immutable `.release/*.json`
changesets for `core` and `craft`; a changeset is optional for ordinary PRs.
After merge, automation aggregates pending summaries into a Release PR that
updates `CHANGELOG.md`. Merging that PR gates each planned module in dependency
order — isolated tidy, build, vet, and race tests — and pushes each tag as its
gate passes. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the contract and
coordinated dependency rules.

For larger work, please open a discussion or draft RFC issue first.

---

## License

[MIT](LICENSE) © GizClaw
