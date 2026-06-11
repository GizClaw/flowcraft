---
layout: default
title: FlowCraft Documentation
---

# FlowCraft

Go SDK for building AI agents with execution primitives, memory substrates,
provider adapters, evaluation harnesses, and voice. Source on
[github.com/GizClaw/flowcraft](https://github.com/GizClaw/flowcraft).

## Migrations

- [`sdk/v0.4.0` + `memory/v0.1.0`](migrations/v0.4.0-memory-split.md) —
  memory-domain packages split into the standalone `memory` module.
- [`sdk/v0.3.0`](migrations/v0.3.0.md) — breaking-change cutover
  closing the v0.2.x deprecation window.

## Layered architecture

The repository is organised as independently released Go modules:

| Layer | Package | Responsibility |
| --- | --- | --- |
| Primitives | `sdk/engine` | Board / Run / Host / Interrupt / Checkpoint contracts |
| DAG executor | `sdk/graph` | Declarative graph runtime (`runner.Runner` implements `engine.Engine`) |
| Orchestration | `sdk/agent` | Agents, observers, deciders, board seeders, handoff DSL |
| Memory substrate | `memory/{sources,views,retrieval,text}` | Source records, derived views, retrieval indexes, text processing |
| Adapters | `sdkx/...` | Concrete provider, tool, checkpoint, and protocol bindings layered on the SDK and memory contracts |
| Voice | `voice/...` | Voice pipeline components |
| Evaluation | `eval/simpleqa` | SimpleQA evaluation harness |

## Repository layout

```
sdk/         Core SDK (interfaces + primitives)
memory/      Sources, views, retrieval, text, execution substrate
sdkx/        Provider, tool, checkpoint, and protocol adapters
voice/       Voice pipeline: STT -> LLM -> TTS
eval/        SimpleQA evaluation harness
examples/    Reference assemblies
tests/       Conformance / quality / e2e suites
```

## Getting started

```bash
go get github.com/GizClaw/flowcraft/sdk@latest
go get github.com/GizClaw/flowcraft/memory@latest
```

See the package-level `doc.go` files for runnable usage snippets:
`sdk/agent/doc.go`, `sdk/engine/doc.go`, `sdk/graph/doc.go`, and
the focused packages under `memory/`.
