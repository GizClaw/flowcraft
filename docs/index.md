---
layout: default
title: FlowCraft Documentation
---

# FlowCraft

Go SDK for building AI agents with long-term memory, knowledge
retrieval, and voice. Source on
[github.com/GizClaw/flowcraft](https://github.com/GizClaw/flowcraft).

## Migrations

- [`sdk/v0.3.0`](migrations/v0.3.0.md) — breaking-change cutover
  closing the v0.2.x deprecation window.

## Layered architecture

The SDK is organised top-down from execution primitives to agent
orchestration:

| Layer | Package | Responsibility |
| --- | --- | --- |
| Primitives | `sdk/engine` | Board / Run / Host / Interrupt / Checkpoint contracts |
| DAG executor | `sdk/graph` | Declarative graph runtime (`runner.Runner` implements `engine.Engine`) |
| Orchestration | `sdk/agent` | Agents, observers, deciders, board seeders, handoff DSL |
| Services | `sdk/{recall,history,knowledge,llm,retrieval,workspace}` | Memory, conversation transcripts, knowledge base, LLM factory, hybrid retrieval, workspace abstraction |
| Adapters | `sdkx/...` | Concrete provider / protocol bindings layered on the SDK contracts |

## Repository layout

```
sdk/      Core SDK (interfaces + primitives)
sdkx/     Extended SDK (concrete adapters with their own go.mod)
voice/    Voice pipeline: STT → LLM → TTS
bench/    Benchmarks (GOWORK=off)
examples/ Reference assemblies
tests/    Conformance / quality / e2e suites
```

## Getting started

```bash
go get github.com/GizClaw/flowcraft/sdk@latest
```

See the package-level `doc.go` files for runnable usage snippets:
`sdk/agent/doc.go`, `sdk/engine/doc.go`, `sdk/graph/doc.go`.
