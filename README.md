<div align="center">

# FlowCraft

**A modular Go toolkit for extensible AI applications, long-term memory, provider backends, and local interactive workflows.**

[![CI](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml/badge.svg)](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/GizClaw/flowcraft/core.svg)](https://pkg.go.dev/github.com/GizClaw/flowcraft/core)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.26%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

FlowCraft is a Go workspace for building and evaluating AI applications without
tying application code to one model provider or execution model. Graphs are one
built-in option, not a required architecture: use the core packages directly, or
start with the forge demo in `examples/forge` for a runnable local workspace.

## Modules

- **`core`** — The single platform module: agent execution, graph, tool,
  model, message, inference, memory contracts, event bus, telemetry,
  workspace, sandbox, deployment/resource assembly, runtime, sessions, and
  delegation contracts.
- **`driver/*`** — Provider adapters built on `core`: Anthropic, Azure,
  ByteDance, DeepSeek, Kimi, MiniMax, OpenAI, and Qwen.
- **`backends/*`** — Platform-specific implementations: SQLite checkpoints
  (`backends/checkpoint`) and the plugin shell (`backends/plugin`); the
  sandbox backends (`bwrap`, `seatbelt`) live in `core/sandbox`.
- **`examples/forge`** — A runnable local workspace demo built on the current
  stack: native deploy/inference/memory scenario configs, an interactive TUI,
  scripted tests, and raid × persona simulation.
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

This mirrors the inference pattern: `core/inference` is generic, and
each provider (`openai`, `deepseek`, …) is a registered factory. Memory
implementations plug in the same way — each registers under its own
`impl:` name with its own parameters (the flowcraft memory module is one
such app-registered implementation); `core` owns only the contracts and
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

## Module map

| Path                                                  | Role                                                                                     | Distribution         |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------- | -------------------- |
| [`core`](core/)                                       | Agent, graph, tool, model, message, inference, memory, event, telemetry, deploy, runtime | Versioned Go module  |
| [`driver`](driver/)                                   | Provider inference adapters                                                              | Versioned Go modules |
| [`backends`](backends/)                       | SQLite checkpoints and plugin shell (sandbox backends live in `core/sandbox`)         | Versioned Go modules |
| [`examples/forge`](examples/forge/)                   | Runnable local workspace demo                                                            | Examples             |
| [`tools/releasegate`](tools/releasegate/)             | Release automation                                                                       | Tools                |
| [`skills/flowcraft-config`](skills/flowcraft-config/) | Codex skill for authoring and validating FlowCraft configs                               | Codex skill          |
| [`skills/flowcraft-plugin`](skills/flowcraft-plugin/) | Codex skill for authoring, loading, and troubleshooting FlowCraft plugins                | Codex skill          |

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

- One runtime for Generate / Embed / Transcription / Realtime, with exact
  `ModelRef` addressing and compile-time capability checks.
- Providers registered as factories: Anthropic, Azure, ByteDance, DeepSeek,
  Kimi, MiniMax, OpenAI, and Qwen.

### Runnable local workspace demo (`examples/forge`)

A runnable demo on the current stack: native scenario documents, an interactive
TUI, scripted tests with per-turn metrics, and raid × persona simulation. See
[`examples/forge/README.md`](examples/forge/README.md) for details.

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
  Transcription / Realtime: deployment config, routing, extensions,
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

The active project surface is `core`, `driver/*`, `backends/*`, and the
forge demo. The `core` module is released independently and remains pre-1.0. Durable
execution contracts (checkpoints, interrupt/resume, scheduling), OTel
instrumentation, and retrieval end-to-end coverage are maintained in-tree;
checkpoint persistence is host-provided through the `CheckpointStore`
contract.

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

This repository is a Go workspace. Active members are `core`, `driver/*`,
`backends/*`, and `examples/forge`; release tooling in `tools/releasegate` builds
standalone with `GOWORK=off`.

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `make ci` should be green.
2. `gofmt -l .` should print nothing.
3. Tests for new features. New behaviour without a test won't merge.
4. Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`,
   `refactor:`, `test:`, `chore:`).

Library releases are declared explicitly with immutable `.release/*.json`
changesets for `core`; a changeset is optional for ordinary PRs. After merge,
automation aggregates pending summaries into a
Release PR that updates `CHANGELOG.md`. Merging that PR runs isolated tidy,
build, vet, and race-test gates before all planned tags are pushed atomically.
See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the contract and coordinated
dependency rules.

For larger work, please open a discussion or draft RFC issue first.

---

## License

[MIT](LICENSE) © GizClaw
