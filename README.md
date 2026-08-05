<div align="center">

# FlowCraft

**A modular Go toolkit for extensible AI applications, long-term memory, provider integrations, and local interactive workflows.**

[![CI](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml/badge.svg)](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/GizClaw/flowcraft/sdk.svg)](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

FlowCraft is a Go workspace for building and evaluating AI applications without
tying application code to one model provider or execution model. Graphs are one
built-in option, not a required architecture: use the SDK packages directly, or
start with the forge demo in `examples/forge` for a runnable local workspace.

## Modules

- **`sdk`** — Core contracts: agent execution, graph, engine, tool, model,
  message, inference, memory capabilities, event bus, telemetry, workspace,
  sandbox, scheduler, and delegation.
- **`memory`** — The flowcraft memory implementation: recall, history,
  knowledge, retrieval indexes, projections, lifecycle maintenance, and the
  background worker.
- **`sdkx`** — Provider adapters and generic assembly layers: inference
  providers (OpenAI, DeepSeek, Qwen, ByteDance, MiniMax, Azure), deploy +
  runtime assembly, tool and workspace configuration, scheduler, and the
  memory assembly, hooks, and renderer.
- **`examples/forge`** — A runnable local workspace demo built on the current
  stack: native deploy/inference/memory scenario configs, an interactive TUI,
  scripted tests, and raid × persona simulation.
- **`tests/conformance`** — Provider conformance suites (credentialed lanes
  self-skip without a repo-root `.env`).
- **`tools/releasegate`** — Release automation: changeset validation, release
  planning, and changelog aggregation.

Library layers are independently versioned Go modules; applications adopt only
the layers they need.

## Memory architecture

`sdk/memory` defines the memory capability contracts — `ContextProvider`,
`TurnSink`, `DocumentSink`, `ContextRenderer`, `Scope`, and `Turn`. The
`memory/` module is **one implementation** of those contracts: it owns its own
settings schema, its own assembly factory, and its own background worker
runtime integration. `sdkx/memory` provides only the generic glue: a
`memory.Assembly` deploy resource that dispatches to implementations by
`impl:` name, the `memory.context` / `memory.turn` agent-lifecycle hooks, and
the GoTemplate context renderer.

This mirrors the inference pattern: `sdkx/inference/config` is generic, and
each provider (`openai`, `deepseek`, …) is a registered factory. Memory
implementations plug in the same way — `impl: flowcraft` selects the bundled
implementation, and another implementation registers under its own name with
its own parameters.

## Quickstart

### Run the forge workspace demo

The fastest way to explore the stack is the runnable demo in
[`examples/forge`](examples/forge/):

```bash
cd examples/forge
go run . help

# Create a workspace from the func_chat scenario and run a scripted test.
go run . workspace create --config func_chat --workspace ./workspace
go run . test -test func_chat/music_direct
```

The demo builds workspaces from native deploy/inference/memory scenario
documents, opens an interactive TUI, and runs scripted tests and raid × persona
simulations. Command reference, scenario layout, and credentials live in
[`examples/forge/README.md`](examples/forge/README.md) (中文版:
[`examples/forge/README_zh.md`](examples/forge/README_zh.md)).

### Embed FlowCraft in a Go service

Use `sdk` directly and add `memory` or `sdkx` when the application needs
recall, history, knowledge, persistence, or provider adapters. Assemble a
deployment from `deploy.yaml` with `sdkx/deploy`, run it with `sdkx/runtime`,
and drive turns through `sdkx/runtime/session`:

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

The SDK defines the execution contracts. Memory and provider adapters compose
those contracts without becoming dependencies of the core.

```
                ┌──────────────────────┐
                │   Your application   │
                └──────────┬───────────┘
                           │
                ┌──────────▼──────────┐
                │        sdkx/        │
                │     providers ·     │
                │     persistence     │
                └──────────┬──────────┘
                           │
                ┌──────────▼──────────┐
                │       memory/       │
                │      recall ·       │
                │      history ·      │
                │      knowledge      │
                └──────────┬──────────┘
                           │
                ┌──────────▼──────────┐
                │        sdk/         │
                │    agent · graph    │
                │    engine · tool    │
                │    event · model    │
                └─────────────────────┘
```

**Layering rule:** `sdk/engine` is a leaf package. It does not import agent,
graph, LLM, or tool packages. Execution implementations satisfy
`engine.Engine`; hosts provide capabilities through `engine.Host`. Memory
contracts live in the SDK, while implementations (the `memory/` module) and
adapters (`sdkx/`) stay outside the core.

## Module map

| Path                                        | Role                                                                             | Distribution         |
| ------------------------------------------- | -------------------------------------------------------------------------------- | -------------------- |
| [`sdk`](sdk/)                               | Agent, graph, engine, tool, model, message, inference, memory, event, telemetry  | Versioned Go module  |
| [`memory`](memory/)                         | Recall, history, knowledge, retrieval, projections, lifecycle, worker            | Versioned Go module  |
| [`sdkx`](sdkx/)                             | Provider adapters + generic assembly (deploy, runtime, tool, memory, scheduler)  | Versioned Go module  |
| [`examples/forge`](examples/forge/)         | Runnable local workspace demo                                                     | Examples            |
| [`tests/conformance`](tests/conformance/)   | Provider conformance suites                                                       | Tests               |
| [`tools/releasegate`](tools/releasegate/)   | Release automation                                                               | Tools               |

## Highlights

### Hybrid memory that actually recalls (`memory/`)

- Three-lane retrieval (BM25 + vector + entity), fused via **Reciprocal Rank
  Fusion** (K=60), then re-weighted by entity-overlap boost, supersede decay,
  and time decay.
- Canonical source → derived view → rebuildable projection → hydrated context,
  with deterministic packing and durable outbox commits.
- Memory as a pluggable implementation: `memory/` is one factory behind the
  `sdk/memory` contracts, and its background worker integrates through its own
  runtime integration.

### Streaming, durable, resumable (`sdk/engine`)

- `Subject`-routed event bus — every step emits structured envelopes.
- `Checkpoint` / `CheckpointStore` contracts — pause and resume an agent
  across restarts.
- `Interrupt` / `Wait` semantics that compose cleanly with `context.Context`.

### Unified inference runtime (`sdk/inference` + `sdkx/inference`)

- One runtime for Generate / Embed / Transcription / Realtime, with exact
  `ModelRef` addressing and compile-time capability checks.
- Providers registered as factories: OpenAI, DeepSeek, Qwen, ByteDance,
  MiniMax, and Azure.

### Runnable local workspace demo (`examples/forge`)

A runnable demo on the current stack: native scenario documents, an interactive
TUI, scripted tests with per-turn metrics, and raid × persona simulation. See
[`examples/forge/README.md`](examples/forge/README.md) for details.

## Documentation

The canonical reference is the per-package `doc.go` files, browsable on
pkg.go.dev:

- [`docs/`](docs/index.md) — guides for graph, tool, event, workspace, sandbox,
  inference, deploy, runtime, and memory.
- [pkg.go.dev/github.com/GizClaw/flowcraft/sdk](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk) — core contracts
- [pkg.go.dev/github.com/GizClaw/flowcraft/memory](https://pkg.go.dev/github.com/GizClaw/flowcraft/memory) — memory implementation
- [pkg.go.dev/github.com/GizClaw/flowcraft/sdkx](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdkx) — adapters and assembly

## Status

The active project surface is `sdk`, `memory`, `sdkx`, and the forge demo.
Library modules are released independently and remain pre-1.0. Durable
execution contracts, Postgres and SQLite checkpoint stores, OTel
instrumentation, and retrieval end-to-end coverage are maintained in-tree.

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

This repository is a Go workspace. Active members are `sdk`, `memory`,
`sdkx`, and `examples/forge`. Some test harnesses intentionally run with
`GOWORK=off` against pinned released modules.

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `make ci` should be green.
2. `gofmt -l .` should print nothing.
3. Tests for new features. New behaviour without a test won't merge.
4. Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`,
   `refactor:`, `test:`, `chore:`).

Library releases are declared explicitly with immutable `.release/*.json`
changesets for `sdk`, `memory`, and `sdkx`; a changeset is optional for
ordinary PRs. After merge, automation aggregates pending summaries into a
Release PR that updates `CHANGELOG.md`. Merging that PR runs isolated tidy,
build, vet, and race-test gates before all planned tags are pushed atomically.
See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the contract and coordinated
dependency rules.

For larger work, please open a discussion or draft RFC issue first.

---

## License

[MIT](LICENSE) © GizClaw
