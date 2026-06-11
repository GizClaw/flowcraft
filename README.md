<div align="center">

# FlowCraft

**Production-grade Go SDK for building AI agents, retrieval-backed memory substrates, provider integrations, and real-time voice pipelines.**

[![CI](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml/badge.svg)](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/GizClaw/flowcraft/sdk.svg)](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk)
[![Go Report Card](https://goreportcard.com/badge/github.com/GizClaw/flowcraft/sdk)](https://goreportcard.com/report/github.com/GizClaw/flowcraft/sdk)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

FlowCraft is a layered toolkit for shipping LLM applications in Go. Pick the layer you need:

- **`sdk`** — Composable primitives: agents, DAG executor, LLM contracts, tools, telemetry, workspaces, and kanban-style multi-agent delegation.
- **`memory`** — Source stores, projected views, lexical/vector retrieval backends, text tokenization, and memory execution substrate.
- **`sdkx`** — Drop-in providers: OpenAI, Anthropic, DeepSeek, MiniMax, Qwen, Azure, ByteDance / Volcengine, plus embeddings, checkpoint stores, extraction, sandbox, telemetry, tools, and workspace adapters.
- **`voice`** — Real-time STT → LLM → TTS pipeline with VAD, barge-in, and WebRTC.
- **`eval`** — Lightweight SimpleQA factuality/calibration harness plus shared eval CLI plumbing.

Everything ships as Go modules with semantic versioning — depend on what you need, ignore the rest.

---

## Why FlowCraft

| You want…                                                    | FlowCraft gives you…                                                                                                      |
| ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------- |
| Strict separation between **engine** and **agent**           | `sdk/engine` is a leaf package; `sdk/agent` orchestrates above it. No "framework is the runtime" coupling.                |
| **Retrieval you can embed or persist**                       | `memory/retrieval` ships workspace, SQLite, Postgres, BBH, scoring, journaling, and namespace tooling behind one interface. |
| **Multi-agent collaboration** without a graph DSL            | `sdk/kanban` exposes any agent as a tool to any other agent — composition is just function calls.                         |
| **Durable graph execution**                                  | `sdk/engine` keeps execution state behind narrow checkpoint contracts, with SQLite/Postgres stores in `sdkx`.             |
| **Voice agents** that don't reinvent VAD                     | `voice/` ships VAD, endpointing, barge-in, WebRTC — wire it to any STT/TTS provider in `sdkx`.                            |
| **Provider portability**                                     | The same agent code runs against OpenAI, Anthropic, DeepSeek, MiniMax, Qwen, Azure, or Volcengine by swapping provider wiring. |

---

## Quickstart

### Library — programmatic SDK usage

For embedding agents directly into a Go service, start with `sdk` and add `sdkx` providers or `memory` retrieval stores as needed. The minimum viable wiring is a graph DAG (`graph.GraphDefinition` + `node.Factory` with `llmnode.Register`) driven by `agent.Run`. See:

- [`sdk/agent/run_test.go`](sdk/agent/run_test.go) — minimal `agent.Run` patterns
- [`examples/voice-pipeline/setup.go`](examples/voice-pipeline/setup.go) — a real graph-runner build wiring an LLM provider + script node

### Retrieval — workspace-backed indexes

Use `memory/retrieval` when you need lexical, vector, sparse, or hybrid search behind a single `retrieval.Index` contract. The default local backend is `memory/retrieval/workspace`, which stores segments in any `sdk/workspace.Workspace`; tests commonly use `sdk/workspace.NewMemWorkspace()` for an in-memory workspace.

### Voice — STT → LLM → TTS

```go
p := voice.NewPipeline(
    sttProvider,                 // any voice/stt backend (e.g. bytedance, …)
    ttsProvider,                 // any voice/tts backend (e.g. minimax, …)
    eng,                         // engine.Engine driving each turn
    agent.Agent{ID: "voice"},
    voice.WithSTTOptions(stt.WithLanguage("zh"), stt.WithTargetSampleRate(16000)),
    voice.WithTTSOptions(tts.WithCodec(audio.CodecMP3)),
)
```

End-to-end: [`examples/voice-pipeline/`](examples/voice-pipeline/) — a runnable WebRTC voice agent.

---

## Architecture

Layered bottom-up. `sdk` is the foundation; `memory` builds on SDK contracts, while `sdkx` and `voice` compose the published SDK surfaces.

```
   ┌──────────────────────────────────────────────────────────────┐
   │                      Your Application                        │
   └────────────┬───────────────────────────────┬─────────────────┘
                │                               │
         ┌──────▼──────┐                 ┌──────▼─────┐
         │    sdkx/    │                 │   voice/   │  WebRTC
         │ providers · │                 │ pipeline   │
         │ checkpoint  │                 └──────┬─────┘
         │ stores      │                        │
         └──────┬──────┘                        │
                │                               │
         ┌──────▼──────┐                 ┌──────▼──────┐
         │   memory/   │                 │    sdk/     │
         │ sources ·   │                 │ agent ·     │
         │ views ·     │                 │ engine ·    │
         │ retrieval · │                 │ graph · llm │
         │ text        │                 │ tool · event│
         └──────┬──────┘                 └──────▲──────┘
                │                               │
                └──────── depends on SDK ───────┘
```

**Layering rule**: `sdk/engine` is a leaf inside `sdk/` — it does NOT import `agent`, `graph`, `llm`, `tool`, or workflow packages. New execution engines plug in by implementing `engine.Engine` against the `Host` capability interface, which keeps the runtime contract narrow. Memory services live in the separate `memory` module and depend on SDK contracts rather than the other way around.

---

## Module map

| Module                                     | What it gives you                                                                                          | Stable        |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------- | ------------- |
| [`sdk`](sdk/)                              | Core primitives — agent, graph DAG, kanban, model, llm, telemetry                                          | yes           |
| [`memory`](memory/)                        | Memory substrate — sources, views, retrieval backends, tokenization, and execution/projector internals     | yes           |
| [`sdkx`](sdkx/)                            | Provider implementations, checkpoint stores, extractors, sandbox, telemetry, tools, workspace adapters     | yes           |
| [`voice`](voice/)                          | Real-time voice pipeline (VAD / STT / LLM / TTS / WebRTC)                                                  | yes           |
| [`eval`](eval/)                            | SimpleQA factuality/calibration harness and shared eval CLI plumbing                                       | —             |
| [`examples/`](examples/)                   | Worked end-to-end examples (currently the voice pipeline)                                                  | —             |
| [`tests/conformance/`](tests/conformance/) | Provider conformance — same surface, every backend                                                         | —             |

---

## Highlights

### Retrieval substrate (`memory/retrieval`)

- Pluggable `retrieval.Index` contract for lexical, vector, sparse, and hybrid search.
- `memory/retrieval/workspace` stores BM25/vector/sparse segments in any `sdk/workspace.Workspace`.
- SQLite, Postgres, BBH, namespace migration, journaling, scoring, and contract-test helpers live under the same module.

### Streaming, durable, resumable (`sdk/engine`)

- `Subject`-routed event bus — every step emits structured envelopes.
- `Checkpoint` / `CheckpointStore` contract — pause and resume an agent across restarts.
- `Interrupt` / `Wait` semantics that compose cleanly with `context.Context`.
- Durable checkpoint stores are available in `sdkx/engine/checkpoint`.

### Voice without the duct tape (`voice`)

- VAD with hysteresis, endpointing, barge-in.
- WebRTC ingress / egress.
- Provider-agnostic: any `sdkx` STT/TTS backend works.

---

## Documentation

The canonical reference is the per-package `doc.go` files, browsable on pkg.go.dev:

- [pkg.go.dev/github.com/GizClaw/flowcraft/sdk](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk) — core primitives (agent, engine, graph, llm, tool, telemetry, …)
- [pkg.go.dev/github.com/GizClaw/flowcraft/memory](https://pkg.go.dev/github.com/GizClaw/flowcraft/memory) — sources, views, retrieval, text packages, and memory execution substrate
- [pkg.go.dev/github.com/GizClaw/flowcraft/sdkx](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdkx) — provider implementations
- [pkg.go.dev/github.com/GizClaw/flowcraft/voice](https://pkg.go.dev/github.com/GizClaw/flowcraft/voice) — voice pipeline

Worked examples live under [`examples/`](examples/). The current runnable example is [`examples/voice-pipeline/`](examples/voice-pipeline/).

---

## Status

`sdk` and `sdkx` are stable and released continuously. `memory` is published as its own module for sources, views, retrieval, text processing, and memory execution substrate. `voice` remains the real-time pipeline layer, and `eval/` now keeps only the SimpleQA baseline plus shared eval CLI plumbing.

API surface is governed by SemVer per module. Breaking changes ship as minor bumps until each module reaches `v1.0.0`.

---

## Building from source

```bash
git clone https://github.com/GizClaw/flowcraft
cd flowcraft
```

This repo is a Go workspace (`go.work`). The active workspace modules are `sdk`, `memory`, `sdkx`, `voice`, and `eval`. To verify the current workspace directly:

```bash
for m in sdk memory sdkx voice eval; do
  (cd "$m" && go test ./...)
done
```

Off-workspace examples and test harnesses pin released versions and run with `GOWORK=off`.

---

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `go test ./...` should pass in each active workspace module (`sdk`, `memory`, `sdkx`, `voice`, `eval`).
2. `gofmt -l .` should print nothing.
3. Tests for new features. New behaviour without a test won't merge.
4. Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`).

For larger work, please open a discussion or draft RFC issue first — it's much faster than reviewing a 5k-line PR cold.

---

## License

[MIT](LICENSE) © GizClaw
