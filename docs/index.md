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

- [Graph Runtime](guides/graph.md) — `sdk/graph`: declarative DAG
  engine, node I/O roles, parallel branches, custom node types.
- [Tool System](guides/tool.md) — `sdk/tool`: LLM function-calling
  contract, Registry / Catalog / Executor split, middleware chain,
  built-in tool adapters and the MCP bridge.
- [Event Bus](guides/event.md) — `sdk/event`: subject-routed
  publish/subscribe, in-process `MemoryBus`, host capability
  wiring, backpressure policies.

### State and execution boundary

- [Workspace](guides/workspace.md) — `sdk/workspace`: per-run
  filesystem abstraction, backends, capabilities, the
  `state vs policy` split vs Sandbox.
- [Sandbox](guides/sandbox.md) — `sdk/sandbox`: agent execution
  boundary, env / net / resources policy, runners (local /
  seatbelt / bwrap), decorators and approval.

### Assembly

- [Inference Runtime](guides/inference.md) — unified Generate / Embed /
  Transcription / Realtime: deployment config, routing, extensions,
  streaming, media intents, hot reload.
- [Deployment Assembly](guides/deploy.md) — `sdkx/deploy`: one YAML
  document + one `Build` call to wire shared resources, named agents,
  engines, and lifecycle hooks.

## Migrations

- [`sdk/v0.5.0` + `sdkx/v0.5.0`](migrations/v0.5.0.md) — unified runtime,
  configuration protocol, sandbox cutover, and the coordinated sdkx
  provider/adapter rebuild (inference providers, deploy/runtime assembly,
  bubblewrap sandbox, A2A remote-proxy engine).
- [`sdk/v0.4.0` + `memory/v0.1.0`](migrations/v0.4.0-memory-split.md) —
  memory-domain packages split into the standalone `memory` module.
- [`sdk/v0.3.0`](migrations/v0.3.0.md) — breaking-change cutover
  closing the v0.2.x deprecation window.

## Layered architecture

The repository is organised as independently released Go modules:

| Layer                | Package                                            | Responsibility                                                                                 |
| -------------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Execution contracts | `sdk/agent`                                        | `Engine` / Board / Run / Host / Interrupt / Checkpoint contracts                               |
| DAG executor         | `sdk/graph`                                        | Declarative graph runtime (`*Graph` implements `agent.Engine`)                                 |
| Agent runtime        | `sdk/agent`                                        | Agents, observers, referees, board seeders, and execution lifecycle                            |
| Delegation contracts | `sdk/delegation`                                   | Backend-neutral target discovery, sync / handoff / async requests, service and host contracts  |
| Async delegation     | `sdkx/delegation/kanban`                           | In-memory `AsyncBackend` / `WorkSource` implementation and operational views                   |
| Scheduling contracts | `sdk/scheduler`                                    | Backend-neutral control plane, leased delivery, typed Client/Worker, and task registration     |
| Local scheduling     | `sdkx/scheduler`                                   | In-process cron, timer, execution queue, lease, memory, and delegation adapters                 |
| Memory services      | `memory/{component,derive,projection,retrieval,lifecycle,worker,sources,views}` | Component graph, derive/projection pipelines, retrieval indexes, lifecycle maintenance, sources and views |
| Adapters             | `sdkx/...`                                         | Concrete provider / protocol bindings layered on the SDK and memory contracts                  |

## Repository layout

```
sdk/         Core SDK (interfaces + primitives)
memory/      Component, derive, projection, retrieval, lifecycle, worker, sources, views
sdkx/        Provider and protocol adapters
examples/    Reference assemblies
```

## Getting started

```bash
go get github.com/GizClaw/flowcraft/sdk@latest
go get github.com/GizClaw/flowcraft/memory@latest
```

See the package-level `doc.go` files for runnable usage snippets:
`sdk/agent/doc.go`, `sdk/graph/doc.go`, and
the focused packages under `memory/`.
