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

- [Graph Engine](guides/graph.md) — `core/agent-graph`: an agent as a
  frontier loop over nodes — declared state with merge rules, conditions
  compiled at build, node kinds, checkpoints and replay.
- [Tool System](guides/tool.md) — `core/tool`: LLM function-calling
  contract, Registry / Catalog / Executor split, middleware chain,
  built-in tool adapters and the MCP bridge.
- [Agent Skills](guides/skill.md) — `core/skill`: SKILL.md discovery
  and parsing, BM25 ranking over names and descriptions, and the
  per-turn `## Skills` section; lifecycle and authoring stay with the
  caller.
- [Application Runtime](guides/runtime.md) — `core/runtime`: one runtime per
  configuration document (provider, tools, sandbox, files, skills, journal)
  and everything it built.
- [Checkpoints](guides/checkpoint.md) — `core/agent`: the store contract —
  a task projection plus an append-only event log, two version counters,
  blobs, and the SQLite backend.
- [Hooks](guides/hooks.md) — `agent.Hooks`: the three host boundaries of a
  turn (`OnSession`, `BeforeTurn`, `AfterTurn`), their failure policy, and
  how a deployment declares its own hook kinds.
- [Media & Attachments](guides/media.md) — `core/message`: the sealed part
  vocabulary, the `core/message/media` value types, and the budgets a
  payload meets on its way into a request.

### State and execution boundary

- [Sandbox](guides/sandbox.md) — `core/sandbox`: agent execution
  boundary, env / net / resources policy, backends (local / seatbelt /
  bwrap / windows), per-call permission, decorators, escalation, and the
  file journal.
- [Remote Sandbox](guides/sandbox-remote.md) — `core/sandbox-remote`: the
  same `Runner` across a host boundary — HTTP or unix socket, cursor reads,
  SSE events, retry rules.

### Assembly

- [Inference Runtime](guides/inference.md) — generate / embed / decide over one
  addressing scheme: the deployment block, host-supplied drivers and selectors,
  model declarations, routing and retry, typed extensions.
- [Deployment Assembly](guides/deploy.md) — `core/deploy`: the deployment
  document, the engine / protocol / hook kind tables, and one `Loader` that
  validates a deployment without building it.
### Delegation

- [Delegation](guides/delegation.md) — `core/delegation`: one agent handing work
  to another — a policy over the session table, derived child sessions,
  sync / async execution, and the lineage a chain keeps.

## Migrations

- [`core/v0.10.0`](migrations/core-v0.10.0.md) — panic recovery is on by
  default and enforced at the fan-out boundaries, `concurrency.limit` must be
  positive, the keychain store is one handle per directory, `Reload` applies
  session limits with the swap, and checkpoints are written incrementally.
- [`core/v0.9.0`](migrations/core-v0.9.0.md) — the typed assembly protocol:
  `Factory[T]`, `Dep[T]`, a pure `deploy.Plan` phase, and typed reads of built
  resources.
- [`core/v0.1.0`](migrations/core-v0.1.0.md) — the breaking cut from
  `sdk`/`sdkx` to the `core` platform module, provider `driver/*`
  modules, and platform-specific `backends/*`.
- [One driver per wire family](migrations/drivers-one-per-wire-family.md) —
  the consolidation that folded the Azure, DeepSeek and Kimi drivers into
  `driver/openai`, moved MiniMax's Messages surface to `driver/anthropic`, and
  removed `driver/qwen`.
- [One driver module](migrations/driver-single-module.md) — the six provider
  modules folded into a single `driver` module; the import paths and the
  `impl:` ids are unchanged and only the `go.mod` requirement moved.
- [Keychain into `core/secret`](migrations/keychain-into-core.md) — the
  `backends/secret` module folded into `core`; the `impl: keychain`
  deployment id is unchanged and only the Go import path moved.

Older `sdk`/`sdkx` migration notes remain in `docs/migrations/` as
historical reference and are not part of the current core migration path.

## Layered architecture

The repository is organised as independently released Go modules, and `core` is
layered: a package imports only from the layers below it.

| Layer | Packages | Responsibility |
| --- | --- | --- |
| Contracts | `core/message` (+`media`), `core/errdefs`, `core/permission`, `core/fsx`, `core/skill`, `core/search` | The vocabulary every other layer speaks: messages, error classes, permissions, file handles, skills, ranking. |
| Inference | `core/inference` (+`model`, `protocol`, `route`, `usage`), `core/inference-openai`, `core/inference-anthropic` | Model contracts, routing, and the two protocol families that ship with the platform. |
| Tools | `core/tool` (+`engine`, `middleware`), `core/tool-builtin/*`, `core/tool-mcp` | The function-calling contract, the projection, and the built-in adapters. |
| Sandbox | `core/sandbox` (+`engine`, `journal`, `netpolicy`), `core/sandbox-*` | The command boundary: contract, policy decorators, backends. |
| Execution | `core/agent` (+`checkpoint`), `core/agent-graph`, `core/agent-a2a`, `core/delegation` | Runs: the engine contract, the graph engine, the A2A client, delegation. |
| Assembly | `core/runtime` | One runtime per configuration document, owning what it built. |
| Deployment | `core/deploy` (+`a2aprotocol`), `core/agent-host` | Documents and kind tables; host bindings, sessions, lifecycle. |
| Observability | `core/telemetry` | Tracing, run records, attributes. |

`core/internal/...` holds module-private helpers and is never importable outside
`core`.

## Repository layout

```text
core/            Platform module (contracts, inference, tools, sandbox, runtime, deploy)
driver/          Provider adapters that are not protocol families (bytedance, dashscope, minimax)
examples/        Reference assemblies: forge (graph demo)
tools/           Release gate, fuzz and stress scripts
```

## Getting started

```bash
go get github.com/GizClaw/flowcraft/core@latest
```

Every package carries a `doc.go` stating what it is and what it deliberately is
not; start there, then read the guide above for the layer you are wiring.
