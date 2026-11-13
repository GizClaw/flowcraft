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
- [Agent Skills](guides/skill.md) — `core/skill`: SKILL.md discovery
  and parsing, BM25 ranking over names and descriptions, and the
  per-turn `## Skills` section; lifecycle and authoring stay with the
  caller.
- [Application Runtime](guides/runtime.md) — `core/runtime`: one runtime per
  configuration document (provider, tools, sandbox, files, skills, journal)
  and everything it built.
- [Checkpoints](guides/checkpoint.md) — `core/agent`: the checkpoint
  store contract, incremental records, and the board write record an
  engine persists at a boundary.
- [External Hooks](guides/hooks.md) — `core/hooks`: the `hooks.json`
  command-hook runner, the frozen event vocabulary, the invocation
  contract, and the trust rules for plugin-contributed sources.
- [Media & Attachments](guides/media.md) — `core/media`:
  attachment classification, image normalization budgets, and the
  `media.attachments` prepare hook; the vocabulary stays in
  `core/message/media`.

### State and execution boundary

- [Sandbox](guides/sandbox.md) — `core/sandbox`: agent execution
  boundary, env / net / resources policy, runners (local /
  seatbelt / bwrap), decorators and approval.

### Assembly

- [Inference Runtime](guides/inference.md) — unified Generate / Embed /
  Transcription (Realtime reserved): deployment config, routing, extensions,
  streaming, media intents, hot reload.
- [Deployment Assembly](guides/deploy.md) — `core/deploy`: the deployment
  document, the engine / protocol / hook kind tables, and one `Loader` that
  validates a deployment without building it.
### Delegation

- [Delegation](guides/delegation.md) — `core/delegation`: backend-neutral
  target discovery, sync / async execution, and the session-bound
  delegation lifecycle.

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

The repository is organised as independently released Go modules:

| Layer                | Package                                            | Responsibility                                                                                 |
| -------------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Execution contracts | `core/agent`                                        | `Engine` / Board / Run / Host / Interrupt / Checkpoint contracts                               |
| DAG executor         | `core/graph`                                        | Declarative graph runtime (`*Graph` implements `agent.Engine`)                                 |
| Agent runtime        | `core/agent`                                        | Agents, observers, referees, board seeders, and execution lifecycle                            |
| Delegation contracts | `core/delegation`                                   | Backend-neutral target discovery, sync / handoff / async requests, service and host contracts  |
| Async delegation     | `core/delegation/kanban`                            | In-memory `AsyncBackend` / `WorkSource` implementation and operational views                   |
| Adapters             | `driver`, `memory`                                  | Concrete provider / protocol bindings layered on core contracts                                |

## Repository layout

```text
core/            Platform module (contracts, deploy, runtime, built-in resources)
driver/          Provider inference adapters (one module)
memory/          Long-term memory backend and its eval harness
examples/        Reference assemblies: forge (workspace demo)
```

## Getting started

```bash
go get github.com/GizClaw/flowcraft/core@latest
```

See the package-level `doc.go` files for runnable usage snippets:
`core/agent/doc.go`, `core/graph/doc.go`, and
the focused packages under `core/`.
