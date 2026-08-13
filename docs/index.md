---
layout: default
title: FlowCraft Documentation
---
# FlowCraft

Go SDK for building AI agents with long-term memory, knowledge
retrieval, runtime orchestration, and voice. Source on
[github.com/GizClaw/flowcraft](https://github.com/GizClaw/flowcraft).

## Guides

### Runtime

- [Graph Runtime](guides/graph.md) — `core/graph`: declarative DAG
  engine, node I/O roles, parallel branches, custom node types.
- [Tool System](guides/tool.md) — `core/tool`: LLM function-calling
  contract, Registry / Catalog / Executor split, middleware chain,
  built-in tool adapters and the MCP bridge.
- [Event Bus](guides/event.md) — `core/event`: subject-routed
  publish/subscribe, in-process `MemoryBus`, host capability
  wiring, backpressure policies.

### State and execution boundary

- [Workspace](guides/workspace.md) — `core/workspace`: per-run
  filesystem abstraction, backends, capabilities, the
  `state vs policy` split vs Sandbox.
- [Sandbox](guides/sandbox.md) — `core/sandbox`: agent execution
  boundary, env / net / resources policy, runners (local /
  seatbelt / bwrap), decorators and approval.

### Assembly

- [Inference Runtime](guides/inference.md) — unified Generate / Embed /
  Transcription / Realtime: deployment config, routing, extensions,
  streaming, media intents, hot reload.
- [Deployment Assembly](guides/deploy.md) — `core/deploy`: one YAML
  document + one `Build` call to wire shared resources, named agents,
  engines, and lifecycle hooks.
- [Resource Protocol](guides/resource.md) — `core/resource`: the
  provider-neutral factory, dependency DAG, loader, and lifecycle phases
  that every deployment resource uses.

## Migrations

- [`core/v0.1.0`](migrations/core-v0.1.0.md) — the breaking cut from
  `sdk`/`sdkx` to the `core` platform module, provider `driver/*`
  modules, and platform-specific `backends/*`.

Older `sdk`/`sdkx` migration notes remain in `docs/migrations/` as
historical reference and are not part of the current core migration path.

## Layered architecture

The repository is organised as independently released Go modules:

| Layer                | Package                                            | Responsibility                                                                                 |
| -------------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Execution contracts | `core/agent`                                        | `Engine` / Board / Run / Host / Interrupt / Checkpoint contracts                               |
| DAG executor         | `core/graph`                                        | Declarative graph runtime (`*Graph` implements `agent.Engine`)                                 |
| Agent runtime        | `core/agent`                                        | Agents, observers, referees, board seeders, and execution lifecycle                            |
| Delegation contracts | `core/delegation`                                   | Backend-neutral target discovery, sync / handoff / async requests, service and host contracts  |
| Async delegation     | `core/delegation/kanban`                            | In-memory `AsyncBackend` / `WorkSource` implementation and operational views                   |
| Adapters             | `driver/*`, `backends/*`                       | Concrete provider / protocol bindings layered on core contracts                                  |

## Repository layout

```
core/            Platform module (contracts, deploy, runtime, built-in resources)
driver/          Provider inference adapters
backends/    Platform-specific sandbox, object-store, and checkpoint backends
examples/        Reference assemblies
```

## Getting started

```bash
go get github.com/GizClaw/flowcraft/core@v0.1.0
```

See the package-level `doc.go` files for runnable usage snippets:
`core/agent/doc.go`, `core/graph/doc.go`, and
the focused packages under `core/`.
